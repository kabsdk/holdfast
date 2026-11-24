# The agent in a container (decision 055): holdfast-agent, the restic Holdfast is tested with, and PostgreSQL's newest
# client, to back up PostgreSQL servers from a pod, such as a CloudNativePG cluster in Kubernetes. It runs as an
# unprivileged user and needs no Kubernetes API access. `make image` builds it, with the binaries copied in beside
# this file; the release publishes it as ghcr.io/kabsdk/holdfast-agent (.github/workflows/image.yml).
#
# It reads /etc/holdfast/agent.yaml (a ConfigMap), keeps its identity, cache and restores in /var/lib/holdfast-agent
# (a volume), and on first start enrolls with the code in /run/holdfast/enroll/code (a Secret).
FROM debian:trixie-slim

ARG HOLDFAST_VERSION=dev
LABEL org.opencontainers.image.title="holdfast-agent" \
	org.opencontainers.image.description="Holdfast's agent with restic and the PostgreSQL client, to back up PostgreSQL from a pod" \
	org.opencontainers.image.source="https://github.com/kabsdk/holdfast" \
	org.opencontainers.image.licenses="Apache-2.0" \
	org.opencontainers.image.version="${HOLDFAST_VERSION}"

ARG PG_MAJOR=18
RUN set -eux; \
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates curl; \
	install -d /usr/share/postgresql-common/pgdg; \
	curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc; \
	. /etc/os-release; \
	echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt ${VERSION_CODENAME}-pgdg main" \
		> /etc/apt/sources.list.d/pgdg.list; \
	apt-get update; \
	apt-get install -y --no-install-recommends postgresql-client-${PG_MAJOR}; \
	apt-get purge -y curl; \
	apt-get autoremove -y; \
	rm -rf /var/lib/apt/lists/*; \
	/usr/lib/postgresql/${PG_MAJOR}/bin/pg_dump --version

COPY holdfast-agent /usr/bin/holdfast-agent
COPY restic /usr/libexec/holdfast/restic
COPY LICENSE THIRD-PARTY-NOTICES /usr/share/doc/holdfast/

RUN set -eux; \
	groupadd -g 65532 holdfast; \
	useradd -u 65532 -g 65532 -d /var/lib/holdfast-agent -M -s /usr/sbin/nologin holdfast; \
	install -d -o 65532 -g 65532 -m 0700 /var/lib/holdfast-agent; \
	/usr/bin/holdfast-agent version; \
	/usr/libexec/holdfast/restic version

USER 65532:65532
ENV HOME=/var/lib/holdfast-agent
WORKDIR /var/lib/holdfast-agent
ENTRYPOINT ["/usr/bin/holdfast-agent", "run", "--config", "/etc/holdfast/agent.yaml", "--enroll-code-file", "/run/holdfast/enroll/code"]
