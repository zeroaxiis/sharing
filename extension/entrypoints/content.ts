export default defineContentScript({
  matches: ['<all_urls>'],
  runAt: 'document_idle',
  main() {
    // TODO(M14): wire the page-context "Send to nearby device" flow — read the
    // current selection / link / image target and forward it to the background
    // script, which owns the daemon connection and drives the transfer.
    //
    // Deliberately inert for Milestone 1: injecting into every page is only
    // justified once there is something to send.
  },
});
