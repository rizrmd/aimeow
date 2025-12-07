#!/bin/sh

echo "Entrypoint script starting..."

# Note: Directories are created by the Go application as needed
# No need to pre-create them here to avoid permission issues with mounted volumes

# Check if binary exists
if [ ! -f "./aimeow" ]; then
    echo "ERROR: aimeow binary not found!"
    ls -la /app/
    exit 1
fi

echo "Binary found. Checking permissions..."
ls -la ./aimeow

# Check if /app/files is writable
echo "Checking /app/files permissions..."
if [ -d "/app/files" ]; then
    if ! touch /app/files/.write_test 2>/dev/null; then
        echo ""
        echo "WARNING: /app/files is not writable by the current user!"
        echo "The volume mount may have incorrect permissions."
        echo ""
        echo "To fix this, run on your host machine:"
        echo "  sudo chown -R 1001:1001 /path/to/your/volume"
        echo "OR when running docker:"
        echo "  docker run -v /path/to/data:/app/files --user 1001:1001 ..."
        echo ""
        echo "The application will still start but may have issues saving data."
        echo ""
    else
        rm -f /app/files/.write_test
        echo "/app/files is writable - OK"
    fi
else
    echo "/app/files does not exist yet, will be created by the application"
fi

echo "Starting application..."
# Start the application
exec ./aimeow