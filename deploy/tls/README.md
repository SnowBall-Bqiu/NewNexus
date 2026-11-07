Place `fullchain.pem` and `privkey.pem` here, or set `NEWNEXUS_TLS_DIR` to an external directory. These files are ignored by Git. The control plane uses the certificate for both REST and HTTP/2 gRPC.

The certificate must contain the public control-plane/Agent DNS name in its SAN, and the full intermediate chain. Mount it read-only. The container runs as UID/GID 65532; grant this group read access to the key without making the key world readable. For a private CA, distribute the CA to Agents with `NEWNEXUS_AGENT_CA_FILE`.

Renew certificates through your normal ACME/deployment process and restart the service after rotation. Do not expose the plaintext development listener to the network.
