.PHONY: all clean build client coordinator mix

all: clean build

clean:
	go clean -i ./...
	rm -rf client coordinator mix

build: client coordinator mix

client:
	CGO_ENABLED=0 go build -a -ldflags '-w -extldflags "-static"' -o client ./vuvuzela-client

coordinator:
	CGO_ENABLED=0 go build -a -ldflags '-w -extldflags "-static"' -o coordinator ./vuvuzela-entry-server

mix:
	CGO_ENABLED=0 go build -a -ldflags '-w -extldflags "-static"' -o mix ./vuvuzela-server
