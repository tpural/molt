# A Paper server with molt-agent: what molt runs as each generation.
# The jar is the same everywhere: build it once, natively.
FROM --platform=$BUILDPLATFORM gradle:9.9.2-jdk25 AS build
ARG VERSION=dev
WORKDIR /src
COPY plugin/ ./
RUN ./gradlew build -q --console=plain -Pversion=${VERSION}

FROM itzg/minecraft-server:2026.11.1-java25
# itzg copies /plugins into the server's plugins on every start.
COPY --from=build /src/build/libs/molt-agent-*.jar /plugins/molt-agent.jar
ENV TYPE=PAPER VERSION=26.3
