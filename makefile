generate-proto:
	protoc --go_out=internal --go-grpc_out=internal proto/bluetooth.proto

preinstall:
	-rm -rf $(HOME)/.config/systemd/user/remote-bluetooth.service

install: preinstall
	$(warning "NOTE! Do not forget to add $$GO/bin to PATH")
	go build -o $(HOME)/go/bin/remote-bluetooth $(shell pwd)/cmd/server/server.go
	mkdir -p $(HOME)/.config/systemd/user
	ln -s $(shell pwd)/remote-bluetooth.service $(HOME)/.config/systemd/user/remote-bluetooth.service

reinstall:
	go build -o $(HOME)/go/bin/remote-bluetooth cmd/server/server.go