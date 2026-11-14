UNIT_DIR := $(HOME)/.config/systemd/user
TOOLS    := .bin/protoc-gen-go .bin/protoc-gen-go-grpc

.PHONY: build test generate install uninstall

build:
	go build -o .bin/remote-bluetooth ./cmd/remote-bluetooth

test:
	go vet ./...
	go test -race ./...

$(TOOLS): go.mod
	go build -o .bin/ google.golang.org/protobuf/cmd/protoc-gen-go google.golang.org/grpc/cmd/protoc-gen-go-grpc

generate: $(TOOLS)
	protoc -I proto \
		--plugin=protoc-gen-go=.bin/protoc-gen-go --go_out=. \
		--go_opt=module=github.com/andree-bjorkgard/remote-bluetooth/v2 \
		--plugin=protoc-gen-go-grpc=.bin/protoc-gen-go-grpc --go-grpc_out=. \
		--go-grpc_opt=module=github.com/andree-bjorkgard/remote-bluetooth/v2 \
		proto/remotebt/v1/remotebt.proto

# Installs the binary to ~/go/bin and the user service. Run
# `remote-bluetooth gen-secret` (once, then copy the secret to the other
# machines) before starting it.
install:
	go install ./cmd/remote-bluetooth
	mkdir -p $(UNIT_DIR)
	install -m 644 remote-bluetooth.service $(UNIT_DIR)/remote-bluetooth.service
	systemctl --user daemon-reload
	@echo "enable with: systemctl --user enable --now remote-bluetooth"

uninstall:
	-systemctl --user disable --now remote-bluetooth
	rm -f $(UNIT_DIR)/remote-bluetooth.service
	systemctl --user daemon-reload
