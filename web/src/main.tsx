import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Toaster } from 'sonner';
import { TooltipProvider } from '@/components/ui/tooltip';
import { initializeTheme } from '@/lib/theme';
import { locale, t } from '@/lib/i18n';
import App from './App';
import './index.css';

initializeTheme();
document.documentElement.lang = locale;
document.title = t('app.title');
document.querySelector('meta[name="description"]')?.setAttribute('content', t('app.metaDescription'));
const queryClient = new QueryClient({ defaultOptions: { queries: { staleTime: 15_000, retry: 1, refetchOnWindowFocus: false }, mutations: { retry: false } } });
createRoot(document.getElementById('root')!).render(<QueryClientProvider client={queryClient}><BrowserRouter><TooltipProvider><App /><Toaster richColors position="bottom-right" closeButton toastOptions={{ className: 'app-toast', closeButtonAriaLabel: t('common.close') }} /></TooltipProvider></BrowserRouter></QueryClientProvider>);
