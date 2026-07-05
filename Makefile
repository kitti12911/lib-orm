# ____________________ Go Command ____________________
tidy:
	go mod tidy

lint: vet golangci-lint markdownlint

ci-test:
	./scripts/ci/go-test.sh

ci-security:
	./scripts/ci/security-scan.sh

ci-markdownlint:
	./scripts/ci/markdownlint.sh

release-plan:
	./scripts/ci/semantic-release-plan.sh

release-publish:
	./scripts/ci/semantic-release-publish.sh

vet:
	go vet ./...

golangci-lint:
	golangci-lint run --timeout=5m

markdownlint:
	./scripts/ci/markdownlint.sh

fmt:
	go fmt ./...

pretty:
	prettier --write "**/*.{md,markdown,yml,yaml,json,jsonc}"

format: fmt pretty

test:
	env CGO_ENABLED=1 go test --race -v ./...

cov:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

fix:
	go fix ./...
