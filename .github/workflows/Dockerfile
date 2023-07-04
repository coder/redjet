FROM ubuntu:latest

# Update the package list and install required packages
RUN apt-get update && apt-get install -y \
    wget \
    curl \
    gcc \
    make

# Set the Go version
ENV GO_VERSION=1.20.0

# Install Go
RUN curl -O https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz && \
    tar -xvf go${GO_VERSION}.linux-amd64.tar.gz && \
    mv go /usr/local

# Set Go Environment Variables
ENV GOROOT=/usr/local/go
ENV GOPATH=$HOME/go
ENV PATH=$GOPATH/bin:$GOROOT/bin:$PATH

# Install Redis
RUN apt-get install -y redis-server
