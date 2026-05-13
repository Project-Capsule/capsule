// Package auth holds capsule's authentication primitives: Ed25519 keypair
// fingerprints, EdDSA-signed JWT minting/verification, the gRPC server
// interceptors that gate every RPC, the boot-time claim window state
// machine that lets a fresh capsule be adopted exactly once, and TLS
// material generation/loading for the self-signed server cert.
//
// Threat model (single-tenant homelab):
//
//   - Network attacker on the capsule's LAN may sniff/replay packets.
//     Defense: TLS 1.3 pins the server's self-signed cert by SHA-256
//     fingerprint (TOFU at adopt time, then strict for every dial).
//   - Network attacker may try to call any RPC without an enrolled key.
//     Defense: default-deny interceptor. The only RPC callable without a
//     bearer token is IdentityService.Adopt, and only while the claim
//     window is open (no keys enrolled AND timer not expired).
//   - Active MITM during first-contact adoption may substitute a forged
//     cert + return a forged tls_fingerprint in AdoptResponse.
//     Defense: the client cross-checks the AdoptResponse fingerprint
//     against the cert it saw on the wire — and the operator confirms
//     against the value printed on the capsule's HDMI banner.
//   - Replay of a captured bearer token within its 60s lifetime.
//     Defense: every JWT carries a random jti; the server's seen-cache
//     rejects any jti it has already accepted.
//   - Lost-all-keys lockout.
//     Defense: the operator with local console access can `touch
//     /perm/capsule/RESET_AUTH; reboot`. Capsuled wipes the keystore on
//     the next boot only if the sentinel mtime is recent — power-cycling
//     alone is not enough.
package auth
