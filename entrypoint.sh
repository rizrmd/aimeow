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

# Don't change ownership of the mount point itself, just create what we need
# The SQLite database will be created in /app/files/aimeow.db
# If there are permission issues with existing files, we'll handle them gracefully

echo "Starting application..."
# Start the application
exec ./aimeow