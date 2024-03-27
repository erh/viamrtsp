
bin/viamrtsp: *.go cmd/module/*.go
	go build -o bin/viamrtsp cmd/module/cmd.go

test:
	go test

lint:
	gofmt -w -s .

updaterdk:
	go get go.viam.com/rdk@latest
	go mod tidy

module: bin/viamrtsp
	tar czf module.tar.gz bin/viamrtsp

clean:
	rm bin/viamrtsp
