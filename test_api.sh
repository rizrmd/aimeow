#!/bin/bash

echo "Testing Aimeow WhatsApp API..."
echo

echo "1. Creating a new client..."
CREATE_RESPONSE=$(curl -s -X POST 'http://localhost:7030/api/v1/clients' -H 'accept: application/json' -d '')
echo "Create Response: $CREATE_RESPONSE"
echo

# Extract client ID from response
CLIENT_ID=$(echo "$CREATE_RESPONSE" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)

if [ -n "$CLIENT_ID" ]; then
    echo "Client ID: $CLIENT_ID"
    
    echo "2. Getting all clients..."
    curl -s -X GET "http://localhost:7030/api/v1/clients" -H 'accept: application/json'
    echo
    echo

    echo "3. Getting specific client details..."
    curl -s -X GET "http://localhost:7030/api/v1/clients/$CLIENT_ID" -H 'accept: application/json'
    echo
    echo

    echo "4. Getting QR code URL..."
    QR_URL=$(echo "$CREATE_RESPONSE" | grep -o '"qrUrl":"[^"]*"' | cut -d'"' -f4)
    echo "QR URL: $QR_URL"
    echo

    echo "5. Health check..."
    curl -s -X GET "http://localhost:7030/health" -H 'accept: application/json'
    echo
else
    echo "Failed to extract client ID from response"
fi

echo
echo "Test completed!"