# Multi-stage: собираем в большом образе с Go, а в итоговый кладём только бинарник.

# --- стадия 1: сборка ---
FROM golang:1.25-alpine AS build
WORKDIR /src
# Сначала только go.mod: пока зависимости не менялись, этот слой и следующий берутся из кэша.
COPY go.mod ./
RUN go mod download
# Потом код: меняется часто, поэтому идёт после зависимостей.
COPY . .
# Статический бинарник — ему не нужна libc, значит подойдёт пустой образ.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /api .

# --- стадия 2: итоговый образ ---
FROM scratch
COPY --from=build /api /api
# Не root: в scratch нет /etc/passwd, поэтому просто числовой uid (nobody).
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/api"]
