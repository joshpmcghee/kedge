
all: messages

messages: *.proto
	PATH="${GOPATH}/bin:${PATH}" protoc \
	  -I. \
		--go_out=plugins=grpc:. \
		*.proto


