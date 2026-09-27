# Two stages: the server binary is built with the committed dashboard bundle, and
# the driver image carries the browser. Nothing here is installed on the host —
# that is the point of the suite (specs/002-control-plane-ui-e2e/plan.md, R-009).

# ---- server ----------------------------------------------------------------
FROM golang:1.27-bookworm AS server-build
WORKDIR /src
# The module cache is a layer of its own so a source-only change does not
# re-download every dependency.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The committed bundle is what the binary serves, so a change under web/ that
# forgot to rebuild it fails here exactly as it would in production.
RUN CGO_ENABLED=0 go build -tags e2e -o /out/dawnbx-server ./cmd/dawnbx-server

# ---- driver ----------------------------------------------------------------
FROM mcr.microsoft.com/playwright:v1.63.0-noble AS driver
WORKDIR /e2e
# The browser is already in this image, which is why nothing is downloaded to a
# developer's machine when the suite runs.
COPY e2e/package.json e2e/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY e2e/ ./
# Compose starts the driver with the suite command; this is only for a bare
# `docker run` against an already-running server.
CMD ["npx", "playwright", "test", "--config", "playwright.config.ts"]
