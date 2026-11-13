# make dev            run locally on a made-up journal, no login, stand-in grader
# make dev ORG=~/org   the same, on your real files (read-only)
# make test           run the tests
# make local          the same as `make dev`, but in Docker
# make hash           make the login settings for .env
# make up / down      start / stop the deployment (app + Caddy)

ORG ?= .dev/org
DEV := ORG_ROOT=$(ORG) DATA_DIR=.dev/data ADDR=127.0.0.1:8080 AUTH_DISABLED=1 \
       KEV_URL=http://127.0.0.1:8009 GRADE_LABEL=fakekev SCAN_INTERVAL=5s

.PHONY: dev test local hash up down clean

dev: .dev/org
	go build -o .dev/ ./cmd/...
	@echo "Open http://127.0.0.1:8080  (Ctrl-C to stop)"
	@.dev/fakekev 127.0.0.1:8009 & trap "kill $$!" EXIT; $(DEV) .dev/introspect

.dev/org:
	mkdir -p .dev/org
	cp -r testdata/org/. .dev/org/
	go run ./cmd/sample .dev/org

test:
	go vet ./...
	go test ./...

local: .dev/org
	docker compose -f compose.local.yaml up --build

hash:
	docker compose run --rm --no-deps introspect hash-password

up:
	docker compose up -d --build

down:
	docker compose down

clean:
	rm -rf .dev
