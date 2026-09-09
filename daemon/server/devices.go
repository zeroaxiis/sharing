package server

import (
	"context"
	"sort"
	"time"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// The device roster: this daemon view of who is on the LAN right now.
//
// Discovery delivers one mDNS event at a time; the extension wants a whole
// snapshot. This file bridges the two. Two details matter more than they look:
//
//   - Pushes are debounced. Every device on the segment re-announces when one
//     of them joins, so a single laptop opening its lid produces a burst of
//     events. Sending one `devices` frame per event would rewrite the UI list a
//     dozen times a second for no new information.
//
//   - `paired` is stamped at read time from the trust store rather than stored
//     on the roster entry. Pairing and discovery change independently, and
//     giving each fact exactly one authority means a pair:forget can never
//     leave a stale `paired: true` behind in the cache.
const (
	// devicesDebounce is how long a change waits for further changes before the
	// snapshot goes out.
	devicesDebounce = 200 * time.Millisecond
	// devicesMaxDelay caps how long a pending push may be deferred. Without it
	// a device re-announcing faster than the debounce window would keep
	// resetting the timer and the UI would never update at all.
	devicesMaxDelay = 1500 * time.Millisecond
	// deviceSweepInterval is how often the roster is aged.
	deviceSweepInterval = 5 * time.Second
	// deviceDropAfter is how long past protocol.DeviceOnlineTTL an unpaired
	// device is kept - shown offline - before it is dropped. Paired devices are
	// never dropped: the user must still be able to see and forget them.
	deviceDropAfter = 2 * protocol.DeviceOnlineTTL
	// lastSeenEpsilon, in milliseconds, is how far LastSeen must move on its own
	// before it counts as a change worth pushing. mDNS refreshes constantly;
	// without this the roster would "change" every few seconds while looking
	// identical to the user.
	lastSeenEpsilon = int64(10 * time.Second / time.Millisecond)
)

// SetDevices replaces the whole roster, as after a full re-browse. It pushes to
// every client only if something actually changed.
func (s *Server) SetDevices(devices []protocol.Device) {
	next := make(map[string]protocol.Device, len(devices))
	for _, d := range devices {
		d = s.normaliseDevice(d)
		if d.ID == "" {
			continue
		}
		next[d.ID] = d
	}

	s.devMu.Lock()
	changed := len(next) != len(s.devByID)
	if !changed {
		for id, d := range next {
			prev, ok := s.devByID[id]
			if !ok || deviceChanged(prev, d) {
				changed = true
				break
			}
		}
	}
	s.devByID = next
	s.devMu.Unlock()

	if changed {
		s.scheduleDevicesBroadcast()
	}
}

// UpsertDevice records one add or update from discovery.
func (s *Server) UpsertDevice(d protocol.Device) {
	d = s.normaliseDevice(d)
	if d.ID == "" {
		return
	}

	s.devMu.Lock()
	prev, existed := s.devByID[d.ID]
	changed := !existed || deviceChanged(prev, d)
	if changed {
		s.devByID[d.ID] = d
	} else {
		// Not worth a push, but the sighting is still real: keep the freshest
		// timestamp so the sweeper cannot age out a device that is right here.
		prev.LastSeen = d.LastSeen
		prev.Status = d.Status
		s.devByID[d.ID] = prev
	}
	s.devMu.Unlock()

	if changed {
		s.log.Debug("device roster updated",
			"deviceId", d.ID, "name", d.Name, "address", d.Address, "port", d.Port)
		s.scheduleDevicesBroadcast()
	}
}

// RemoveDevice drops a device discovery says has gone away.
func (s *Server) RemoveDevice(deviceID string) {
	if deviceID == "" {
		return
	}
	s.devMu.Lock()
	_, existed := s.devByID[deviceID]
	delete(s.devByID, deviceID)
	s.devMu.Unlock()

	if existed {
		s.log.Debug("device left", "deviceId", deviceID)
		s.scheduleDevicesBroadcast()
	}
}

// Devices renders the roster as the extension sees it: discovered devices
// stamped with their trust status, plus any paired device that is not on the
// air right now, so a device the user already trusts never silently vanishes
// from the list.
func (s *Server) Devices() []protocol.Device {
	paired := map[string]struct{}{}
	if s.trust != nil {
		paired = s.trust.PairedIDs()
	}
	nowMS := nowMillis()

	s.devMu.Lock()
	out := make([]protocol.Device, 0, len(s.devByID)+len(paired))
	seen := make(map[string]struct{}, len(s.devByID))
	for id, d := range s.devByID {
		_, isPaired := paired[id]
		d.Paired = isPaired
		// Status is derived from the clock, not from whatever the last event
		// claimed, so a snapshot is honest even between sweeps.
		if staleAt(d.LastSeen, nowMS) {
			d.Status = protocol.StatusOffline
		}
		out = append(out, d)
		seen[id] = struct{}{}
	}
	s.devMu.Unlock()

	if s.trust != nil {
		for _, p := range s.trust.List() {
			if _, ok := seen[p.DeviceID]; ok {
				continue
			}
			out = append(out, protocol.Device{
				ID:       p.DeviceID,
				Name:     p.Name,
				Platform: protocol.PlatformUnknown,
				Status:   protocol.StatusOffline,
				Paired:   true,
				LastSeen: timeMillis(p.LastSeen),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// device looks one device up in the roster, with paired and status stamped the
// same way Devices does.
func (s *Server) device(deviceID string) (protocol.Device, bool) {
	if deviceID == "" {
		return protocol.Device{}, false
	}
	s.devMu.Lock()
	d, ok := s.devByID[deviceID]
	s.devMu.Unlock()
	if !ok {
		return protocol.Device{}, false
	}
	if s.trust != nil {
		d.Paired = s.trust.IsPaired(deviceID)
	}
	if staleAt(d.LastSeen, nowMillis()) {
		d.Status = protocol.StatusOffline
	}
	return d, true
}

// knownDevice reports whether a device id can plausibly be routed to: either it
// is on the air, or it is paired and may still be reachable over a live socket.
func (s *Server) knownDevice(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	if s.trust != nil && s.trust.IsPaired(deviceID) {
		return true
	}
	s.devMu.Lock()
	_, ok := s.devByID[deviceID]
	s.devMu.Unlock()
	return ok
}

// SweepDevices ages the roster: past the TTL a device goes offline, and well
// past it an unpaired device is forgotten. It pushes only if something moved.
//
// This is what notices a laptop that closed its lid: mDNS goodbye packets are
// best effort, so nothing but the clock reports an ungraceful departure.
func (s *Server) SweepDevices() {
	nowMS := nowMillis()
	dropBefore := nowMS - int64(deviceDropAfter/time.Millisecond)

	var paired map[string]struct{}
	if s.trust != nil {
		paired = s.trust.PairedIDs()
	}

	changed := false
	s.devMu.Lock()
	for id, d := range s.devByID {
		if _, isPaired := paired[id]; !isPaired && d.LastSeen < dropBefore {
			delete(s.devByID, id)
			changed = true
			continue
		}
		if d.Status != protocol.StatusOffline && staleAt(d.LastSeen, nowMS) {
			d.Status = protocol.StatusOffline
			s.devByID[id] = d
			changed = true
		}
	}
	s.devMu.Unlock()

	if changed {
		s.scheduleDevicesBroadcast()
	}
}

// BroadcastDevices pushes the roster to every client immediately, cancelling
// any debounce in flight. Called when something other than discovery changed
// the answer: a pairing completing, or a device being forgotten.
func (s *Server) BroadcastDevices() {
	s.stopDeviceTimer()
	s.Broadcast(protocol.NewDevices(s.Devices()))
}

// runDeviceSweeper ages the roster until ctx is cancelled.
func (s *Server) runDeviceSweeper(ctx context.Context) {
	ticker := time.NewTicker(deviceSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepDevices()
		}
	}
}

// scheduleDevicesBroadcast coalesces a burst of roster changes into one push.
func (s *Server) scheduleDevicesBroadcast() {
	s.devMu.Lock()
	defer s.devMu.Unlock()

	if s.devTimer == nil {
		s.devWindow = time.Now()
		s.devTimer = time.AfterFunc(devicesDebounce, s.flushDevices)
		return
	}
	// Once the window has been open too long, stop extending it and let the
	// pending timer fire; otherwise a chatty device could defer the push
	// indefinitely.
	if time.Since(s.devWindow) < devicesMaxDelay {
		s.devTimer.Reset(devicesDebounce)
	}
}

func (s *Server) flushDevices() {
	s.devMu.Lock()
	s.devTimer = nil
	s.devMu.Unlock()
	s.Broadcast(protocol.NewDevices(s.Devices()))
}

// stopDeviceTimer cancels a pending debounced push. Safe to call at any time,
// including on a server that never scheduled one.
func (s *Server) stopDeviceTimer() {
	s.devMu.Lock()
	t := s.devTimer
	s.devTimer = nil
	s.devMu.Unlock()
	if t != nil {
		t.Stop()
	}
}

// normaliseDevice fills in the fields a discovery event may leave blank and
// drops anything that must never enter the roster.
func (s *Server) normaliseDevice(d protocol.Device) protocol.Device {
	// Discovery self-filters on the TXT id, but a second check here is cheap
	// and means a bug there cannot make this daemon offer to pair with itself.
	if d.ID == s.cfg.DeviceID {
		return protocol.Device{}
	}
	if d.LastSeen <= 0 {
		d.LastSeen = nowMillis()
	}
	if d.Status == "" {
		d.Status = protocol.StatusOnline
	}
	if d.Platform == "" {
		d.Platform = protocol.PlatformUnknown
	}
	if d.Name == "" {
		d.Name = d.ID
	}
	// Paired is never taken from the wire; it is stamped from the trust store.
	d.Paired = false
	return d
}

// deviceChanged reports whether two sightings of the same device differ in a
// way the UI should be told about.
func deviceChanged(prev, next protocol.Device) bool {
	if prev.Name != next.Name || prev.Address != next.Address || prev.Port != next.Port ||
		prev.Platform != next.Platform || prev.Version != next.Version || prev.Status != next.Status {
		return true
	}
	return next.LastSeen-prev.LastSeen >= lastSeenEpsilon
}

func staleAt(lastSeenMS, nowMS int64) bool {
	return nowMS-lastSeenMS > int64(protocol.DeviceOnlineTTL/time.Millisecond)
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func timeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
