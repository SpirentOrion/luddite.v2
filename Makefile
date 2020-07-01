BUILD_PATH ?= .

ifdef COMSPEC
EXAMPLE := example\\example.exe
else
EXAMPLE := example/example
endif

.PHONY: all vet build test clean

all: vet build test

vet:
	cd $(BUILD_PATH) && go vet -all -composites=false ./...

build:
	cd $(BUILD_PATH) && go build .
	cd $(BUILD_PATH) && go build -o $(EXAMPLE) ./example/...

test:
	cd $(BUILD_PATH) && go test -race

clean:
	cd $(BUILD_PATH) && go clean
	rm -f $(EXAMPLE)
