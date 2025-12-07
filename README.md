# Aimeow WhatsApp API

REST API for managing multiple WhatsApp clients with Swagger documentation.

## Latest Update: 2025-11-25
- Enhanced Docker debugging and health checks
- Added comprehensive message and image sending capabilities
- Fixed deployment issues with improved error handling

## Quick Start

### Local Development
```bash
# Build and run
go run main.go

# Or build executable
go build -o aimeow .
./aimeow
```

### Docker

```bash
# Build the Docker image
docker build -t aimeow .

# Run with proper volume permissions
docker run -d \
  -p 7030:7030 \
  -v $(pwd)/data:/app/files \
  --name aimeow \
  aimeow

# IMPORTANT: Fix volume permissions if you get permission errors
# The container runs as user 1001:1001 for security
# If you see "permission denied" errors, run:
sudo chown -R 1001:1001 ./data

# Or run with matching user ID:
docker run -d \
  -p 7030:7030 \
  -v $(pwd)/data:/app/files \
  --user $(id -u):$(id -g) \
  --name aimeow \
  aimeow
```

## API Endpoints

Base URL: `http://localhost:7030/api/v1`

### Clients

- `POST /clients/new` - Create new WhatsApp client
- `GET /clients` - List all clients
- `GET /clients/{id}` - Get client details
- `GET /clients/{id}/qr` - Get QR code (terminal format)
- `GET /clients/{id}/messages` - Get client messages
- `DELETE /clients/{id}` - Delete client

### Documentation

- Swagger UI: http://localhost:7030/swagger/index.html
- Health check: http://localhost:7030/health
- QR Code: http://localhost:7030/qr?client_id=YOUR_CLIENT_ID

## Usage Examples

### Create a new client
```bash
curl -X POST http://localhost:7030/api/v1/clients/new
```
Response:
```json
{
  "id": "75335d94-c1bb-4d11-a42c-fb24f2e02d5d",
  "qrUrl": "http://localhost:7030/qr?client_id=75335d94-c1bb-4d11-a42c-fb24f2e02d5d"
}
```

### Get QR code for pairing (HTML)
```bash
# Open in browser
open http://localhost:7030/qr?client_id=75335d94-c1bb-4d11-a42c-fb24f2e02d5d

# Or get terminal format
curl http://localhost:7030/api/v1/clients/75335d94-c1bb-4d11-a42c-fb24f2e02d5d/qr
```

### List all clients
```bash
curl http://localhost:7030/api/v1/clients
```
Response includes phone number once connected:
```json
[
  {
    "id": "75335d94-c1bb-4d11-a42c-fb24f2e02d5d",
    "phone": "1234567890",
    "isConnected": true,
    "connectedAt": "2024-12-07T14:30:00Z",
    "messageCount": 5
  }
]
```

### Get messages
```bash
curl http://localhost:7030/api/v1/clients/75335d94-c1bb-4d11-a42c-fb24f2e02d5d/messages?limit=10
```

## Features

- Multi-client support
- Real-time QR code generation
- Message history tracking
- Client connection status
- Auto-reconnection for existing sessions
- Graceful client deletion
- Swagger API documentation