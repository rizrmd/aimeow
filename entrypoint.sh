#!/bin/sh

# Ensure files directory exists and has correct permissions
mkdir -p /app/files
chown -R appuser:appgroup /app/files

# Start the application
exec ./aimeow