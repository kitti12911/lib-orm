# ____________________ Go Command ____________________
tidy:
	go mod tidy

fmt:
	go fmt ./...

test:
	env CGO_ENABLE=1 go test --race -v ./...

cov:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

fix:
	go fix ./...
