# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This repository contains documentation and research for building a distributed SQLite platform called "LiteVFS" or "DistSQLite" - a zero-code-change sidecar solution that makes any SQLite application globally distributed. The project is in the conceptual/planning phase.

## Key Concepts

### Architecture Goal
Build a transparent sidecar process that intercepts SQLite operations via VFS (Virtual File System) layer, enabling:
- Zero code changes required from applications
- Local-speed reads and writes
- Intelligent bulk synchronization using sqlite3_rsync
- Dynamic write lease management across regions

### Core Components (Planned)
1. **SQLite VFS Module** (C) - Intercepts SQLite operations in-process
2. **Sidecar Process** (Go) - Handles replication and coordination
3. **Write Lease Coordinator** (Go/Elixir) - Manages primary assignment
4. **Replication Engine** - Uses sqlite3_rsync for bulk operations
5. **Litestream Integration** - Continuous backup to S3

### Competitive Advantage
- 10-100x faster than Turso for bulk operations
- No application rewrites needed (vs. Turso's complete API change)
- Works with any existing SQLite application in any language

## Project Status

This is currently a **research and planning repository** containing:
- Technical architecture discussions
- Implementation strategy documents
- Research on SQLite VFS, FUSE, and distributed systems
- Competitive analysis against Turso, LibSQL, and LiteFS

## Documentation Files

- `convo.md` - Initial research conversation on libsql replication and sqlite3_rsync
- `convo2.md` - Detailed architectural discussions and implementation strategy
- `litefs_context.txt` - LiteFS codebase analysis for understanding proven patterns

## No Build Commands

This repository contains only documentation - there are no build, test, or lint commands as this is a planning/research phase project.

## Implementation Notes

When implementing this project:
- Use proven tools (Litestream, sqlite3_rsync) rather than building from scratch
- Focus on VFS interception for transparency, avoid FUSE due to performance limitations
- Leverage Fly.io's infrastructure for coordination and deployment
- Implement dynamic lease migration based on write patterns
- Ensure fence tokens prevent split-brain scenarios during lease transfers

## Future Development

The planned implementation timeline is 8 weeks:
- Weeks 1-2: Core VFS module
- Weeks 3-4: Sidecar process
- Weeks 5-6: Distribution logic
- Weeks 7-8: Production hardening

The goal is to make SQLite distribution as simple as adding one sidecar process to any deployment.