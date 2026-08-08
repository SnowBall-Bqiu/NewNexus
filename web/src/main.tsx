import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Toaster } from 'sonner';
import { Tooltip } from '@base-ui-components/react/tooltip';
import App from './App';
import './styles.css';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: 10_000, refetchOnWindowFocus: false, retry: 1 },
    mutations: { retry: 0 },
  },
});

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <Tooltip.Provider delay={500} closeDelay={0} timeout={400}>
        <BrowserRouter>
          <App />
          <Toaster position="bottom-right" richColors closeButton />
        </BrowserRouter>
      </Tooltip.Provider>
    </QueryClientProvider>
  </React.StrictMode>,
);
