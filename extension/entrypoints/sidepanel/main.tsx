import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';
import './App.css';

const container = document.getElementById('root');
if (!container) throw new Error('side panel root element is missing');

ReactDOM.createRoot(container).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
