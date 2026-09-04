import { createKumoToastManager } from '@cloudflare/kumo';
export const toastManager = createKumoToastManager();
export const toast = { success: (title: string, description?: string) => toastManager.add({ title, description, variant: 'success' }), error: (title: string, description?: string) => toastManager.add({ title, description, variant: 'error' }), warning: (title: string, description?: string) => toastManager.add({ title, description, variant: 'warning' }), info: (title: string, description?: string) => toastManager.add({ title, description, variant: 'info' }) };
