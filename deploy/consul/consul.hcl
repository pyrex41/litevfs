# Consul configuration for DistSQLite on Fly.io
datacenter = "fly-${FLY_REGION}"
data_dir = "/app/data/consul"
log_level = "INFO"
server = true

# Fly.io networking
bind_addr = "0.0.0.0"
client_addr = "0.0.0.0"

# Multi-region cluster formation
retry_join = [
  "consul-ord.internal",
  "consul-iad.internal", 
  "consul-lax.internal",
  "consul-fra.internal"
]

# Bootstrap configuration
bootstrap_expect = 3  # Expect 3 servers across regions

# Networking and ports
ports {
  grpc = 8502
  http = 8500
  dns = 8600
}

# Performance tuning for Fly.io
performance {
  raft_multiplier = 1  # Fly.io has low latency
}

# ACLs and security (if needed)
acl = {
  enabled = false  # Disabled for testing, enable for production
  default_policy = "allow"
}

# Service discovery
connect {
  enabled = true
}

# UI for debugging
ui_config {
  enabled = true
}

# Logging
log_file = "/app/logs/consul.log"
log_rotate_duration = "24h"
log_rotate_max_files = 7

# Telemetry for monitoring
telemetry {
  statsd_address = "localhost:8125"
  disable_hostname = true
}

# Snapshot configuration  
snapshot_agent {
  snapshot {
    interval = "1h"
    retain = 24
    stale = false
  }
  http_addr = "127.0.0.1:8500"
  log_level = "INFO"
  log_file = "/app/logs/consul-snapshot.log"
}