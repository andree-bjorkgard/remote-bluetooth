generate-proto:
	protoc --go_out=internal --go-grpc_out=internal proto/bluetooth.proto

preinstall:
	-rm -rf $(HOME)/.config/systemd/user/remotebluetooth.service

install: preinstall
	$(warning "NOTE! Do not forget to add $GO/bin to PATH")
	go build -o $(GOPATH)/bin/remotebluetooth cmd/server/main.go
	mkdir -p $(HOME)/.config/systemd/user
	ln -s $(shell pwd)/repo-indexer.service $(HOME)/.config/systemd/user/remotebluetooth.service

reinstall:
	go build -o $(GOPATH)/bin/remotebluetooth cmd/server/main.go