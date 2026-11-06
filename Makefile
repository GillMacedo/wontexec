.PHONY: test lint demo demo-pie demo-arch demo-fixed tool demo-files

test:
	test -z "$$(gofmt -l plugin tests examples)"
	go vet ./...
	go test -race -count=1 ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

TOOL := bin/wontexec

tool:
	@go build -o $(TOOL) ./plugin/scripts

# Two made-up images (OCI layout tars) and three builds of examples/hello, all for
# fixed targets so the output is the same on any machine. The chmod keeps the
# printed mode the same whatever the umask is.
demo-files: tool
	@go run ./examples/mkimage -o bin/scratch-amd64.tar -arch amd64 -entrypoint /app
	@go run ./examples/mkimage -o bin/static-amd64.tar -arch amd64 -entrypoint /app -user nonroot -files certs,tzdata,passwd
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildmode=pie -o bin/hello-pie ./examples/hello
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/hello-arm64 ./examples/hello
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/hello ./examples/hello
	@chmod 0755 bin/hello-pie bin/hello-arm64 bin/hello

demo: demo-pie demo-arch demo-fixed

# These two have problems on purpose, so exit 1 is the expected result.
demo-pie: demo-files
	@$(TOOL) --binary bin/hello-pie bin/scratch-amd64.tar || test $$? -eq 1

demo-arch: demo-files
	@$(TOOL) --binary bin/hello-arm64 bin/static-amd64.tar || test $$? -eq 1

# The fixed build in an image with certificates and zone data. It must stay quiet and exit 0.
demo-fixed: demo-files
	@$(TOOL) --binary bin/hello bin/static-amd64.tar
