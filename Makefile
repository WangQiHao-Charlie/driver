PROTO_SRC=api/proto/runtime/v1/driver.proto

.PHONY: proto
proto:
    PATH=$$(go env GOPATH)/bin:$$PATH \
    protoc -I . \
        --go_out=. --go_opt=paths=source_relative \
        --go-grpc_out=. --go-grpc_opt=paths=source_relative \
        $(PROTO_SRC)
