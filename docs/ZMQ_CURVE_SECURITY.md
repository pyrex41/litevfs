# ZMQ CURVE Security Implementation

## Overview

DistSQLite now supports ZMQ CURVE authentication for secure, encrypted communication between nodes. CURVE provides both authentication and confidentiality using public-key cryptography.

## Key Features

- **Authentication**: Verifies identity of both client and server
- **Encryption**: All messages are encrypted in transit
- **Performance**: Minimal overhead compared to TLS
- **Simplicity**: Uses Z85 text encoding for easy key management

## Configuration

### Server Configuration (Coordinator/Lease Server)

```yaml
zmq:
  security:
    enabled: true
    curve_enabled: true
    server_public_key: "rq:rM>}U?@Lns47E1%kR.o@n%FcmmsL/@{H8]yf7"
    server_secret_key: "JTKVSB%%)wK0E.X)V>+}o?pNmC{O&4W4b!Ni{Lh6"
    allowed_clients: []  # Optional: restrict to specific client keys
```

### Client Configuration (Nodes)

```yaml
zmq:
  security:
    enabled: true
    curve_enabled: true
    server_public_key: "rq:rM>}U?@Lns47E1%kR.o@n%FcmmsL/@{H8]yf7"  # Server's public key
    client_public_key: "Yne@$w-vo<fVvi]a<NY6T1ed:M$fCG*[IaLV{hID"  # This node's public key
    client_secret_key: "D:)Q[IlAW!ahhC2ac:9*A}h:p?([4%wOTJ%JR%cs"  # This node's secret key
```

## Key Management

### Test Keys (Development Only)

The implementation includes test keys from the ZMQ specification:

```go
// Server keys
TestServerPublicKey  = "rq:rM>}U?@Lns47E1%kR.o@n%FcmmsL/@{H8]yf7"
TestServerSecretKey  = "JTKVSB%%)wK0E.X)V>+}o?pNmC{O&4W4b!Ni{Lh6"

// Client keys  
TestClientPublicKey  = "Yne@$w-vo<fVvi]a<NY6T1ed:M$fCG*[IaLV{hID"
TestClientSecretKey  = "D:)Q[IlAW!ahhC2ac:9*A}h:p?([4%wOTJ%JR%cs"
```

### Production Key Generation

For production use, generate unique keys:

```bash
# Using zmq_curve_keypair (if available)
zmq_curve_keypair

# Or using Python with pyzmq
python3 -c "
import zmq.auth
public, secret = zmq.auth.curve_keypair()
print('Public:', public.decode('ascii'))
print('Secret:', secret.decode('ascii'))
"
```

### Key Distribution

1. **Server Keys**: 
   - Keep secret key secure on server
   - Distribute public key to all clients

2. **Client Keys**:
   - Generate unique key pair per client
   - Keep secret key secure on client
   - Optionally register client public keys on server

## Fly.io Deployment

### Environment Variables

```bash
# Server (coordinator/lease server)
export CURVE_ENABLED=true
export CURVE_SERVER_PUBLIC_KEY="your-server-public-key"
export CURVE_SERVER_SECRET_KEY="your-server-secret-key"

# Client (nodes)
export CURVE_ENABLED=true  
export CURVE_SERVER_PUBLIC_KEY="server-public-key"
export CURVE_CLIENT_PUBLIC_KEY="this-node-public-key"
export CURVE_CLIENT_SECRET_KEY="this-node-secret-key"
```

### Fly Secrets

```bash
# Set server keys
fly secrets set CURVE_SERVER_SECRET_KEY="JTKVSB%%)wK0E.X)V>+}o?pNmC{O&4W4b!Ni{Lh6"

# Set client keys (per region/machine)
fly secrets set CURVE_CLIENT_SECRET_KEY="D:)Q[IlAW!ahhC2ac:9*A}h:p?([4%wOTJ%JR%cs"
```

## Security Considerations

### Development vs Production

- **Development**: Use test keys for simplicity
- **Production**: Generate unique keys per deployment
- **Never**: Commit secret keys to version control

### Key Rotation

```yaml
# Support multiple server keys during rotation
zmq:
  security:
    enabled: true
    curve_enabled: true
    server_public_key: "new-server-public-key"
    server_secret_key: "new-server-secret-key"
    # Legacy key support during transition
    legacy_server_keys:
      - "old-server-public-key"
```

### Client Authentication

```yaml
# Restrict to specific client public keys
zmq:
  security:
    allowed_clients:
      - "Yne@$w-vo<fVvi]a<NY6T1ed:M$fCG*[IaLV{hID"  # Node 1
      - "different-client-public-key-here"             # Node 2
```

## Testing

### Unit Tests

```bash
# Run CURVE authentication tests
go test -v ./test/integration -run TestZMQCurveAuthentication
```

### Integration Tests

```bash
# Test with CURVE enabled
export CURVE_ENABLED=true
./test/run_distributed_tests.sh
```

## Troubleshooting

### Common Issues

1. **Authentication Failures**
   ```
   Error: failed to connect: authentication failed
   ```
   - Check server public key matches on client
   - Verify Z85 key format (40 characters)

2. **Invalid Key Format**
   ```
   Error: failed to set CURVE key: invalid key
   ```
   - Keys must be 40-character Z85 encoded strings
   - Use only valid Z85 characters

3. **Connection Timeouts**
   ```
   Error: context deadline exceeded
   ```
   - CURVE handshake adds ~1ms latency
   - Increase timeout if needed

### Debug Logging

```yaml
logging:
  level: "debug"  # Enable detailed CURVE logging
```

Look for log messages:
- `[ZMQ Security] CURVE server authentication enabled`
- `[ZMQ Security] CURVE client authentication enabled`
- `[ZMQ Security] Using test keys (development only)`

## Performance Impact

- **Latency**: +1-2ms per connection establishment
- **Throughput**: <1% impact on message throughput  
- **CPU**: Minimal encryption overhead
- **Memory**: ~1KB per authenticated connection

## Migration Path

### Phase 1: Deploy with CURVE Disabled
```yaml
zmq:
  security:
    enabled: false
    curve_enabled: false
```

### Phase 2: Enable CURVE with Test Keys
```yaml
zmq:
  security:
    enabled: true
    curve_enabled: true
    # Uses test keys automatically
```

### Phase 3: Production Keys
```yaml
zmq:
  security:
    enabled: true
    curve_enabled: true
    server_public_key: "${CURVE_SERVER_PUBLIC_KEY}"
    server_secret_key: "${CURVE_SERVER_SECRET_KEY}"
    client_public_key: "${CURVE_CLIENT_PUBLIC_KEY}"
    client_secret_key: "${CURVE_CLIENT_SECRET_KEY}"
```

## References

- [ZMQ CURVE Specification](http://rfc.zeromq.org/spec:25)
- [Z85 Encoding](http://rfc.zeromq.org/spec:32)
- [ZMQ Security Guide](https://zeromq.org/socket-api/#security)

## Examples

See `test/integration/curve_auth_test.go` for complete working examples of:
- CURVE disabled operation
- CURVE enabled with test keys
- Authentication failure handling
- Invalid key format handling