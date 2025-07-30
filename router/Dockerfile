FROM golang:1.24.5-alpine
WORKDIR /app
COPY . .
RUN go build -o router .
CMD ["./router"]