run:
	go mod tidy -compat=1.17
	CGO_ENABLED=1 go build -v -o ./main.exe .
	./main.exe

run2:
	go mod tidy -compat=1.17
	CGO_ENABLED=1 go build -v -o ./short .
	./short
