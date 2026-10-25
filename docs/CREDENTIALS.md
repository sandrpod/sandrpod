# Credentials the agent may use but not copy

> Opt-in. `-credential-mask=OPENAI_API_KEY,GITHUB_TOKEN` — off by default, and
> requires egress enforcing. When it is on, the agent's TLS is terminated for
> allow-listed hosts, and that sentence has consequences: read §2 before §1.

An agent that can reach a paid API needs the key. If the key is in its
environment, it can read it, print it, and send it anywhere the egress policy
allows — and "anywhere allowed" is a wider set than "where it was meant to go".
This feature makes the key usable and unreadable at the same time:

```
sandbox:  OPENAI_API_KEY=fake_value_4b1f…        (a sentinel)
   │
   ├─ request with "Authorization: Bearer fake_value_4b1f…"
   ▼
egress proxy (terminates TLS, substitutes on the way out)
   │
   └─ request with "Authorization: Bearer sk-real-…"     → upstream
```

The real bytes live in the agent's memory and in the request the proxy sends.
They are never in the sandbox's environment, its files, its command lines, or
its audit log.

---

## 1. What is masked

| Surface | How | Where |
|---|---|---|
| Environment variables | the sandbox is given sentinels instead of values | `Masker.Env()`, merged over the inherited environment |
| Request headers | every header value is scanned; sentinels are replaced | `Masker.substituteHeaders` |
| Request bodies | up to 16 MiB, length-preserving so `Content-Length` stays valid | `serveTerminated` |
| Files (`~/.aws/credentials`, `~/.netrc`, …) | a rewritten copy is bound read-only over the original | `-credential-mask-files`, `Policy.ReadSubstitute` |
| AWS SigV4 | the signature is recomputed with the real secret | `Masker.ResignAWS` |

The sentinel is `fake_value_<uuid4>`, padded to the real value's length when the
real value is longer. Length matters: a substitution that keeps the byte count
lets the proxy forward the request without re-framing, and re-framing is where
chunked-vs-length bugs live.

SigV4 gets special treatment because it is the one credential that cannot simply
be swapped in place: the signature is an HMAC over the whole request, so a
request signed with the sentinel key has to be re-signed with the real secret.
The trigger is the sentinel appearing in the `Credential=` scope. The
implementation is checked against AWS's published example signature
(`pkg/egress/sigv4_test.go`) — a re-signer that is subtly wrong produces exactly
the same error as a wrong secret, which is a bug that hides for months.

File masking on Linux binds the fake over the real path, so the real bytes are
not in the sandbox's namespace at all. The substitution is mounted *after* the
hardlock masks, which is what makes the documented example work at all: the
default seeds hardlock `~/.aws` as a whole, so a substitution applied before that
mask would have been wiped out and the tool would have found no file. A sentinel
file inside a masked directory exposes nothing, so the order costs nothing.

If the sentinel copy is missing (cleaned up, rotated, unreadable) the spawn is
**refused** rather than run with the real file readable: a credential that was
meant to be masked and is not is the one failure this feature cannot degrade
into. On macOS there is no mount to substitute under, so the path is denied
instead: the tool breaks rather than reading the real value.

---

## 2. What it costs, stated plainly

TLS termination means the sandbox's connection no longer ends at the upstream.
For every terminated host:

- the certificate the sandbox sees is signed by **SandrPod's CA**, not by the
  upstream. Certificate pinning breaks. `mTLS` to the upstream cannot work (only
  the client holds the client certificate, and we are now the client);
- the proxy sees plaintext, including the real credential it just substituted
  in. The proxy — and the CA private key on disk — become trust anchors for that
  traffic, at least as sensitive as the credentials themselves;
- anything inside the sandbox that reasons "I verified the peer end to end" is
  wrong.

Two escape hatches exist because those costs are real:

- **`-credential-terminate-exclude=<domain>`** keeps a host as an opaque tunnel:
  no termination, no substitution, normal end-to-end TLS. Use it for mTLS and
  pinning. Those hosts get no masking — that is the trade, not an oversight.
- **`-credential-allow-plaintext`** permits substitution on cleartext HTTP. Off
  by default because it puts the real value on the wire; without it, a request
  carrying a sentinel over plain HTTP is refused with
  `X-Deny-Reason: masked_credential_cleartext` rather than leaked or silently
  broken.

It is worth being precise about which trust relationship changes. The sandbox
never trusted the host — the host can read the same filesystem, replace
binaries, and watch the process. What this feature changes is the relationship
between the **sandbox and the upstream**: it is deliberately cut, and moved to
the proxy. If a workload needs that relationship intact, exclude it.

---

## 3. Configuration

```bash
sandrpod-agent -api-url=https://api.example.com -name=my-laptop -token=… \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json \
  -credential-mask=OPENAI_API_KEY,ANTHROPIC_API_KEY,GITHUB_TOKEN \
  -credential-mask-files=~/.aws/credentials,~/.netrc \
  -credential-terminate-exclude=internal.corp.example     # mTLS upstream
```

| Flag | Env | Meaning |
|---|---|---|
| `-credential-mask` | `SANDRPOD_CREDENTIAL_MASK` | Credential names to mask; values are read from the agent's own environment |
| `-credential-mask-files` | `SANDRPOD_CREDENTIAL_MASK_FILES` | Files rewritten and bound over (`~` allowed) |
| `-credential-ca-dir` | `SANDRPOD_CREDENTIAL_CA_DIR` | Where the CA lives (default `~/.sandrpod/egress-ca`) |
| `-credential-terminate-exclude` | `SANDRPOD_CREDENTIAL_TERMINATE_EXCLUDE` | Domains never terminated |
| `-credential-allow-plaintext` | `SANDRPOD_CREDENTIAL_ALLOW_PLAINTEXT` | Permit substitution over HTTP |

The CA is generated on first run and **persisted**: a CA that changed every
restart would force every tool in the sandbox to re-trust, and break anything
that cached a certificate. `ca.key` is written `0600`; treat it as a secret.

The agent points the sandbox's runtimes at the CA through the environment
(`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`,
`GIT_SSL_CAINFO`, `AWS_CA_BUNDLE`, `DENO_CERT`, `PIP_CERT`), all at one bundle
file containing our CA **and** the system roots — so a tool can still verify a
host that was excluded or never went through the proxy.

On Linux the bundle carries the system roots. On macOS the system store is a
keychain view that cannot always be enumerated, in which case the bundle holds
our CA alone; termination then covers every allowed 443 host, and an excluded
host needs its own roots. The startup log says which case you are in.

`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` are linked
automatically when they are masked, which is what enables re-signing.

---

## 4. Audit

Terminated connections are marked, and credential use is recorded by name:

```json
{"source":"egress","kind":"connect","host":"api.openai.com","port":443,
 "decision":"allow","terminated":true,"credentials":["OPENAI_API_KEY"]}
```

The value never appears. Two properties make the record worth keeping: a reviewer
can see which conversations sent which credential where, and the `terminated`
flag makes the loss of end-to-end TLS a visible event rather than a footnote.

---

## 5. Verifying it

Unit and integration tests run anywhere:

```bash
go test ./pkg/egress -run 'Masking|ResignAWS|MaskFile|CA_'
```

They cover: the sentinel shape and length preservation, a value that would be
ambiguous (refused), headers and bodies substituted against a real TLS upstream,
the upstream reading the real value while the client only ever had the sentinel,
an excluded host staying a genuinely opaque tunnel (the client's handshake fails
because we are not the upstream), cleartext sentinels refused, SigV4 re-signed to
AWS's published signature, and the CA persisted across restarts.

The end-to-end case runs a real sandboxed process in a real network namespace,
in a privileged container:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./pkg/sandbox -o /tmp/sandbox.test
docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run MaskedCredential'
```

It asserts three things at once: the process has no route of its own, its request
reaches the proxy through the relay, and the upstream reads the real credential —
while the sandbox's own output never contains it.

---

## 6. What this does not do

- **It does not stop a determined read of the real value.** The proxy has it, the
  agent has it, anything that can read the agent's memory or the CA key has it.
  This protects against the *model*, which is the thing that writes your
  commands.
- **It does not mask what the upstream echoes back.** A poorly behaved service
  that returns the key in a response body hands it to the sandbox. Nothing here
  inspects responses.
- **It does not cover h2 inside a terminated tunnel.** The terminated path
  negotiates HTTP/1.1 only, deliberately: negotiating h2 would silently skip
  substitution for every request that used it.
- **It does not mask a credential that is not in the agent's environment or a
  listed file.** There is no discovery: you name what gets masked.
- **It does not make macOS file masking work.** On macOS a masked file is
  denied, not substituted — the tool breaks instead of leaking.

See also: [EGRESS_POLICY.md](EGRESS_POLICY.md) (the allow-list this rides on),
[OS_SANDBOX.md](OS_SANDBOX.md) (the kernel floor under all of it),
[PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) (the gate and the audit
trail).