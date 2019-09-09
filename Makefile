.PHONY: all clean build client coordinator mixer

all: clean build

clean:
	go clean -i ./...
	rm -rf vuvuclient vuvucoordinator vuvumixer

build: client coordinator mixer

client:
	go build -o vuvuclient ./cmd/vuvuzela-client

coordinator:
	go build -o vuvucoordinator ./cmd/vuvuzela-coordinator

mixer:
	go build -o vuvumixer ./cmd/vuvuzela-mixer
