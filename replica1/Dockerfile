FROM golang:1.22-alpine

WORKDIR /app

RUN go install github.com/air-verse/air@v1.52.3

COPY go.mod ./
COPY . .

CMD ["air", "-c", ".air.toml"]
