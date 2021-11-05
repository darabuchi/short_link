run:
	go mod tidy -compat=1.17
	CGO_ENABLED=1 go build -v -o ./main.exe ./cmd/.
	./main.exe
