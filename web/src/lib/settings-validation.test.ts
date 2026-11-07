import { describe, expect, it } from 'vitest';
import { settingsPayload, settingsValues, validateHttpTemplate, validateSettings } from './settings-validation';

const fields = [
  { key: 'probe.retries', label: 'Retries', type: 'number', min: 0, max: 3 },
  { key: 'probe.minimum_success_rate', label: 'Success', type: 'number', min: 0, max: 1 },
  { key: 'retention.raw_days', label: 'Retention', type: 'number', min: 1 },
];
describe('settings validation', () => {
  it('allows zero, fractional success rates and valid scoring weights', () => {
    expect(validateSettings({ 'probe.retries': 0, 'probe.minimum_success_rate': .8, 'score.weights': { tcp: 0, tls: .5 } }, fields)).toEqual({});
  });
  it('blocks empty, nonfinite, fractional integer fields and server range violations', () => {
    for (const value of [NaN, Infinity, '', null, -1, 4, .5]) expect(validateSettings({ 'probe.retries': value }, fields)).toHaveProperty('probe.retries');
    expect(validateSettings({ 'retention.raw_days': 3651 }, fields)).toHaveProperty('retention.raw_days');
  });
  it('rejects invalid and all-zero weights before serializing a request', () => {
    expect(validateSettings({ 'score.weights': { tcp: NaN } }, fields)).toHaveProperty('score.weights.tcp');
    expect(validateSettings({ 'score.weights': { tcp: -1 } }, fields)).toHaveProperty('score.weights.tcp');
    expect(validateSettings({ 'score.weights': { tcp: 0 } }, fields)).toHaveProperty('score.weights');
  });
  it('validates JSON headers and status arrays before converting edited text into request values', () => {
    const values = { 'probe.method': 'POST', 'probe.path': '/health', 'probe.headers': '{"X_Custom_Header":"value"}', 'probe.expected_statuses': '[200,204]' };
    expect(validateHttpTemplate(values)).toEqual({});
    expect(settingsPayload(values)).toMatchObject({ 'probe.headers': { X_Custom_Header: 'value' }, 'probe.expected_statuses': [200, 204] });
    for (const headers of ['{', '[]', '{"Bad Name":"value"}', '{"X-Test":2}', JSON.stringify({ 'X-Test': 'a\r\nb' })]) expect(validateHttpTemplate({ 'probe.headers': headers })).toHaveProperty('probe.headers');
    for (const statuses of ['{', '{}', '[]', '[199]', '[600]', '[200.5]', '["200"]']) expect(validateHttpTemplate({ 'probe.expected_statuses': statuses })).toHaveProperty('probe.expected_statuses');
  });
  it('rejects unsupported methods and paths that change the query or fragment', () => {
    expect(validateHttpTemplate({ 'probe.method': 'TRACE' })).toHaveProperty('probe.method');
    for (const path of ['health', '/health?check=1', '/health#ok', '/health\n']) expect(validateHttpTemplate({ 'probe.path': path })).toHaveProperty('probe.path');
    expect(validateHttpTemplate({ 'probe.method': 'PATCH', 'probe.path': '' })).toEqual({});
  });
  it('never puts server masks or unchanged secrets into a settings write', () => {
    const mask = '\u2022'.repeat(8);
    const values = settingsValues([{ key: 'alerts.smtp_password', value: mask }, { key: 'alerts.webhook_secret', value: mask }, { key: 'probe.retries', value: 2 }]);
    expect(values).toEqual({ 'alerts.smtp_password': '', 'alerts.webhook_secret': '', 'probe.retries': 2 });
    expect(settingsPayload({ ...values, 'probe.retries': 3 })).toEqual({ 'probe.retries': 3 });
    expect(settingsPayload({ 'alerts.smtp_password': mask, 'alerts.webhook_secret': mask })).toEqual({});
    expect(settingsPayload({ 'alerts.smtp_password': 'replacement', 'alerts.webhook_secret': 'replacement-secret' })).toEqual({ 'alerts.smtp_password': 'replacement', 'alerts.webhook_secret': 'replacement-secret' });
  });

  it('requires a real boolean for self-signed Agent TLS and keeps disabled false in the payload', () => {
    const key = 'agent_tls.self_signed_enabled';
    for (const value of ['true', 'false', 0, 1, null, {}, []]) expect(validateSettings({ [key]: value }, fields)[key]).toBeTruthy();
    for (const value of [false, true]) {
      expect(validateSettings({ [key]: value }, fields)).toEqual({});
      expect(settingsPayload({ [key]: value, 'alerts.smtp_password': '', 'alerts.webhook_secret': '' })).toEqual({ [key]: value });
    }
    expect(validateSettings({}, fields)).toEqual({});
  });

  it('accepts independent API, console and Agent addresses and empty inherited values', () => {
    const addresses = {
      'addresses.api_origin': 'https://api.example.com:8443/',
      'addresses.console_origin': 'https://console.example.com',
      'addresses.agent_endpoint': 'agent.example.com:443',
    };
    expect(validateSettings(addresses, fields)).toEqual({});
    expect(settingsPayload(addresses)).toEqual(addresses);
    const inherited = Object.fromEntries(Object.keys(addresses).map((key) => [key, '']));
    expect(validateSettings(inherited, fields)).toEqual({});
    expect(settingsPayload(inherited)).toEqual(inherited);
  });

  it.each(['addresses.api_origin', 'addresses.console_origin', 'addresses.agent_endpoint'])('only accepts strings for %s', (key) => {
    for (const value of [null, false, 443, [], {}, ['https://api.example.com']]) {
      expect(validateSettings({ [key]: value }, fields)[key]).toBeTruthy();
    }
    expect(validateSettings({ [key]: undefined }, fields)).toEqual({});
  });

  it.each(['addresses.api_origin', 'addresses.console_origin'])('rejects non-origin addresses for %s', (key) => {
    for (const value of ['api.example.com', 'https://api.example.com/api', 'https://api.example.com?test=1', 'https://api.example.com#test', 'https://user:password@api.example.com', 'https://api.example.com:0', 'https://api.example.com:65536', 'ftp://api.example.com']) {
      expect(validateSettings({ [key]: value }, fields)[key]).toBeTruthy();
    }
    expect(validateSettings({ [key]: 'http://127.0.0.1:8080' }, fields)).toEqual({});
  });

  it('requires host:port rather than an HTTP URL or path for the Agent address', () => {
    const key = 'addresses.agent_endpoint';
    for (const value of ['agent.example.com', 'https://agent.example.com:443', 'agent.example.com:443/api', 'agent.example.com:0', 'agent.example.com:65536', '0.0.0.0:9091', '[::]:9091']) {
      expect(validateSettings({ [key]: value }, fields)[key]).toBeTruthy();
    }
    for (const value of ['agent.example.com:443', '127.0.0.1:9091', '[2001:db8::1]:443']) {
      expect(validateSettings({ [key]: value }, fields)).toEqual({});
    }
  });
});
