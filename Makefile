.PHONY: test bench lint cover clean

test:
	go test -race ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./...

lint:
	go vet ./...
	gofmt -l .

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

clean:
	rm -f coverage.out *.db *.test
