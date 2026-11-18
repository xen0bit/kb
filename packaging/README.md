# packaging

`apt-signing-key.asc` goes here: the public half of the `APT_SIGNING_KEY`
secret that signs the apt repository. The release workflow refuses to
publish when the secret's fingerprint does not match this file, so a rotated
or mistyped secret fails the build instead of breaking `apt update` for every
user.

```bash
gpg --quick-generate-key "kb apt repository" ed25519 sign never
gpg --armor --export-secret-keys <FPR>   # → GitHub secret APT_SIGNING_KEY
gpg --armor --export <FPR> > packaging/apt-signing-key.asc
```
