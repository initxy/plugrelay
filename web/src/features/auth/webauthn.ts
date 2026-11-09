import type { PasskeyCreationOptions, PasskeyCredential, PasskeyRequestOptions } from "@/lib/api/types";

/** base64url (no padding) ⇄ bytes; the API carries every WebAuthn binary field this way. */
export function fromB64url(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4);
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

export function toB64url(b: ArrayBuffer | ArrayBufferView): string {
  const bytes = b instanceof ArrayBuffer ? new Uint8Array(b) : new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
  let bin = "";
  for (const x of bytes) bin += String.fromCharCode(x);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function passkeysSupported(): boolean {
  return typeof window !== "undefined" && typeof window.PublicKeyCredential === "function" && !!navigator.credentials;
}

const descriptor = (d: { id: string; transports?: string[] }): PublicKeyCredentialDescriptor => ({
  type: "public-key",
  id: fromB64url(d.id),
  ...(d.transports?.length ? { transports: d.transports as AuthenticatorTransport[] } : {}),
});

/** Run navigator.credentials.create with the relay's creation options. */
export async function createPasskey(o: PasskeyCreationOptions): Promise<PasskeyCredential> {
  const cred = (await navigator.credentials.create({
    publicKey: {
      challenge: fromB64url(o.challenge),
      rp: o.rp,
      user: { id: fromB64url(o.user.id), name: o.user.name, displayName: o.user.displayName },
      pubKeyCredParams: o.pubKeyCredParams,
      timeout: o.timeout,
      attestation: o.attestation,
      excludeCredentials: o.excludeCredentials.map(descriptor),
      authenticatorSelection: o.authenticatorSelection as AuthenticatorSelectionCriteria,
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("no credential");
  const r = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: "public-key",
    response: {
      clientDataJSON: toB64url(r.clientDataJSON),
      attestationObject: toB64url(r.attestationObject),
      transports: typeof r.getTransports === "function" ? r.getTransports() : [],
    },
  };
}

/** Run navigator.credentials.get with the relay's request options. */
export async function getPasskey(o: PasskeyRequestOptions): Promise<PasskeyCredential> {
  const cred = (await navigator.credentials.get({
    publicKey: {
      challenge: fromB64url(o.challenge),
      rpId: o.rpId,
      timeout: o.timeout,
      userVerification: o.userVerification,
      allowCredentials: o.allowCredentials.map(descriptor),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("no credential");
  const r = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: "public-key",
    response: {
      clientDataJSON: toB64url(r.clientDataJSON),
      authenticatorData: toB64url(r.authenticatorData),
      signature: toB64url(r.signature),
      ...(r.userHandle ? { userHandle: toB64url(r.userHandle) } : {}),
    },
  };
}

/** True when the person closed or timed out the browser's passkey prompt. */
export function passkeyCancelled(e: unknown): boolean {
  return e instanceof DOMException && (e.name === "NotAllowedError" || e.name === "AbortError");
}
