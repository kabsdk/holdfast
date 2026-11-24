# The e2e suite's image: Go, to build and run the suite, and PostgreSQL from its own repository for the PostgreSQL
# jobs' tests (decision 055): the server and client of version 18, which the agent's container image has, and the
# server and client of version 16: a client older than a server (P001), and a server older than the client, which
# restores must load into. make e2e builds it once; Docker's cache makes later builds instant.
ARG GO_IMAGE=golang:1.26
FROM ${GO_IMAGE}
RUN set -eux; \
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates curl; \
	install -d /usr/share/postgresql-common/pgdg /etc/postgresql-common; \
	curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc; \
	. /etc/os-release; \
	echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt ${VERSION_CODENAME}-pgdg main" \
		> /etc/apt/sources.list.d/pgdg.list; \
	echo "create_main_cluster = false" > /etc/postgresql-common/createcluster.conf; \
	apt-get update; \
	apt-get install -y --no-install-recommends postgresql-18 postgresql-16; \
	rm -rf /var/lib/apt/lists/*; \
	/usr/lib/postgresql/18/bin/postgres --version; \
	/usr/lib/postgresql/16/bin/postgres --version
