FROM golang:1.20 AS builder

RUN mkdir /src
WORKDIR /src

ADD . .
ARG VERSION
RUN \
	--mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 \
	go build \
	-tags lambda.norpc \
	-ldflags="-X main.Version=$VERSION" -v \
	-o /lambda ./cmd/lambda/

FROM public.ecr.aws/lambda/provided:al2

LABEL org.opencontainers.image.source=https://github.com/pentops/o5-github-lambda

COPY --from=builder /lambda /lambda

ENTRYPOINT ["/lambda"]
