FROM golang:1.22-bookworm

# The trim-pathed go binary finds GOROOT via /proc/self/exe; set it explicitly so
# `go` works even where that detection is unavailable. build-template.sh bakes the
# image's Config.Env (incl. this) into the guest.
ENV GOROOT=/usr/local/go

# git and ca-certificates already ship in the golang bookworm image.
WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh
