# CRUSH.md - Aimeow WhatsApp Bot

## Project Overview

Aimeow is a WhatsApp bot built with Go using the `whatsmeow` library. The project provides a basic WhatsApp client framework with message handling capabilities. Today is November 2025. 

## Essential Commands

### Build & Run
```bash
go run main.go              # Build and run the application
go build -o aimeow .        # Build executable to aimeow
go mod tidy                 # Clean up dependencies
go test ./...               # Run all tests (when tests exist)
```

### Development
```bash
go fmt ./...                # Format all Go files
go vet ./...                # Static analysis
go install                  # Install to $GOPATH/bin
```

## Project Structure

```
/
├── main.go                 # Main application entry point
├── go.mod                  # Go module definition
├── go.sum                  # Dependency checksums
├── .gitignore              # Git ignore patterns
├── .specify/               # Project management templates
├── .codex/                 # Prompt templates
└── *.db                    # SQLite databases (created at runtime)
```

## Code Organization

- **main.go**: Contains the main WhatsApp client logic, multi-client management, event handlers, and database setup
- **Event handling**: Uses type assertion to handle different WhatsApp events with client-specific logging
- **Database**: SQLite with foreign keys enabled via `sqlstore`
- **Authentication**: QR code-based login for new sessions, auto-reconnect for existing sessions
- **Multi-client**: Supports multiple WhatsApp instances simultaneously, each with unique device IDs

## Dependencies

Key dependencies from `go.mod`:
- `go.mau.fi/whatsmeow`: WhatsApp Web client library
- `go.mau.fi/whatsmeow/store`: Device storage types
- `go.mau.fi/whatsmeow/store/sqlstore`: SQL database container
- `go.mau.fi/whatsmeow/types/events`: Event types for handling
- `go.mau.fi/libsignal`: Signal protocol implementation
- `github.com/rs/zerolog`: Structured logging
- `filippo.io/edwards25519`: Edwards25519 cryptographic operations
- `github.com/google/uuid`: UUID generation
- `github.com/mattn/go-sqlite3`: SQLite database driver
- `github.com/mdp/qrterminal/v3`: Terminal QR code rendering

## Database Requirements

- **Connector**: Must import SQLite3 connector (`github.com/mattn/go-sqlite3`)
- **File**: Creates `examplestore.db` for session storage
- **Foreign Keys**: Enabled for data integrity

## Code Patterns & Conventions

### Error Handling
- Use `panic()` for fatal initialization errors
- Follow Go idioms for error returns

### Logging
- Use `waLog.Stdout` for whatsmeow-specific logging
- Component names: "Database", "Client"
- Log level: "DEBUG"

### Event Handling
- Type assertion on `evt.(type)` for event routing
- Currently handles `*events.Message` events
- Expand with additional event types as needed

## Important Notes

### Device Types
- Use `*store.Device` for device storage (not `*sqlstore.Device`)
- Import both `go.mau.fi/whatsmeow/store` and `go.mau.fi/whatsmeow/store/sqlstore`

### QR Code Rendering
- Uses `github.com/mdp/qrterminal/v3` for terminal QR code display
- `qrterminal.Generate()` with Large (L) size for clear scanning
- Each client shows QR code with device ID prefix for identification

### Session Management
- Multi-client support with `GetAllDevices()` to retrieve all stored devices
- Each client has unique device ID for identification
- Concurrent connection using goroutines and sync.WaitGroup
- Graceful shutdown disconnects all clients simultaneously

## Gotchas

1. **Database files**: `*.db` files are gitignored but created at runtime
2. **Graceful shutdown**: App waits for Ctrl+C before disconnecting all clients
3. **Debug logging**: Currently set to DEBUG level, may be verbose
4. **Multi-client**: All clients connect concurrently using goroutines

## Testing

No tests currently exist. When adding tests:
- Place in `*_test.go` files
- Use table-driven tests for event handlers
- Mock external dependencies for unit tests

## Configuration

Currently hardcoded values:
- Database file: `examplestore.db`
- Log level: "DEBUG"
- Foreign keys: enabled

Future improvements may include configuration files or environment variables.