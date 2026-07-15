PLUGIN := glm-vision-combo
VERSION ?= 0.4.3

test:
	go test ./...
	go test -race ./...

build-linux-amd64:
	mkdir -p dist
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o dist/$(PLUGIN).so .

package-linux-amd64: build-linux-amd64
	cd dist && zip -q $(PLUGIN)_$(VERSION)_linux_amd64.zip $(PLUGIN).so
	cd dist && shasum -a 256 $(PLUGIN)_$(VERSION)_linux_amd64.zip > checksums.txt
