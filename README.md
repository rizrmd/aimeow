# Aimeow WhatsApp API

REST API for managing multiple WhatsApp clients with Swagger documentation.

## Quick Start

```bash
# Build and run
go run main.go

# Or build executable
go build -o aimeow .
./aimeow
```

## API Endpoints

Base URL: `http://localhost:8080/api/v1`

### Clients

- `POST /clients` - Create new WhatsApp client
- `GET /clients` - List all clients
- `GET /clients/{id}` - Get client details
- `GET /clients/{id}/qr` - Get QR code (terminal format)
- `GET /clients/{id}/messages` - Get client messages
- `DELETE /clients/{id}` - Delete client

### Documentation

- Swagger UI: http://localhost:8080/swagger/index.html
- Health check: http://localhost:8080/health

## Usage Examples

### Create a new client
```bash
curl -X POST http://localhost:8080/api/v1/clients
```
Response:
```json
{
  "id": "12345678-1234-1234-1234-123456789abc",
  "qrCode": "2@abc123..."
}
```

### Get QR code for pairing
```bash
curl http://localhost:8080/api/v1/clients/12345678-1234-1234-1234-123456789abc/qr
```

### List all clients
```bash
curl http://localhost:8080/api/v1/clients
```

### Get messages
```bash
curl http://localhost:8080/api/v1/clients/12345678-1234-1234-1234-123456789abc/messages?limit=10
```

## Features

- Multi-client support
- Real-time QR code generation
- Message history tracking
- Client connection status
- Auto-reconnection for existing sessions
- Graceful client deletion
- Swagger API documentation