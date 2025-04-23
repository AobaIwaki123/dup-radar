#!/bin/bash

# Load environment variables from .env file if it exists
if [ -f .env ]; then
    export $(cat .env | xargs)
fi

# Start ngrok to expose the local server
ngrok http 8080 &

# Wait for ngrok to initialize
sleep 5

# Start the local server
go run cmd/dup-radar/main.go