// RFC 6238 TOTP (SHA-1, 6 digits, 30 s) for the company owner's MFA
// enrolment in setup (orders need an MFA-verified company_admin session,
// 03 k-mfa). Same parameters as cmd/koro/smoke_integration_test.go.
import k6crypto from 'k6/crypto';

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

// base32Decode decodes an RFC 4648 base32 string (no padding needed).
export function base32Decode(s) {
  const clean = s.toUpperCase().replace(/=+$/, '').replace(/\s+/g, '');
  const out = [];
  let bits = 0;
  let value = 0;
  for (const ch of clean) {
    const idx = ALPHABET.indexOf(ch);
    if (idx < 0) throw new Error(`base32: bad character ${ch}`);
    value = (value << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return new Uint8Array(out);
}

// totp returns the code of the 30-second step `step` (default: now).
export function totp(secretBase32, step) {
  const counter = step === undefined ? Math.floor(Date.now() / 1000 / 30) : step;
  const msg = new Uint8Array(8);
  let c = counter;
  for (let i = 7; i >= 0; i--) {
    msg[i] = c % 256;
    c = Math.floor(c / 256);
  }
  const mac = k6crypto.hmac('sha1', base32Decode(secretBase32).buffer, msg.buffer, 'hex');
  const bytes = [];
  for (let i = 0; i < mac.length; i += 2) bytes.push(parseInt(mac.slice(i, i + 2), 16));
  const off = bytes[bytes.length - 1] & 0x0f;
  const bin = ((bytes[off] & 0x7f) << 24) | (bytes[off + 1] << 16) | (bytes[off + 2] << 8) | bytes[off + 3];
  return String(bin % 1000000).padStart(6, '0');
}
