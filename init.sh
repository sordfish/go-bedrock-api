#!/bin/sh
set -e

echo "Updating apk and installing dependencies..."
apk update && apk add --no-cache git

echo "Cloning repository..."
git clone https://github.com/sordfish/go-bedrock-api.git /app
cd /app

echo "Running application..."
go run main.go
