import { describe, expect, it } from 'vitest';
import { authPayload } from './LoginPage';

describe('auth request payloads', () => {
  it('does not send the login-only code during first-time setup', () => {
    expect(authPayload(true, 'admin', 'strong-password', '123456')).toEqual({ username: 'admin', password: 'strong-password' });
  });

  it('keeps the TOTP code for normal login', () => {
    expect(authPayload(false, 'admin', 'strong-password', '123456')).toEqual({ username: 'admin', password: 'strong-password', code: '123456' });
  });
});
