FROM golang:1.17

WORKDIR /usr/src/app

# pre-copy/cache go.mod for pre-downloading dependencies and only redownloading them in subsequent builds if they change
COPY go.mod ./
RUN go mod download && go mod verify

COPY . .
RUN go build -ldflags="-X 'main.Version=$version' -X 'main.Time=$time'" -v -o /usr/local/bin/main ./main.go

CMD ["main"]