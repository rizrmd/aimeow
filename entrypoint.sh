#!/bin/sh

# Create subdirectories within files for application data
mkdir -p /app/files/sessions /app/files/media /app/files/qrcodes

# Don't change ownership of the mount point itself, just create what we need
# The SQLite database will be created in /app/files/aimeow.db
# If there are permission issues with existing files, we'll handle them gracefully

# Start the application
exec ./aimeow