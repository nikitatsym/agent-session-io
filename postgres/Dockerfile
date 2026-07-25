ARG PGVECTOR_COMMIT=159b79aaad5983fb7459c1e3df2897fbb2d11788
ARG PG_TEXTSEARCH_COMMIT=578ff529894992fb9e67cae4c69424e65c84868e

FROM postgres:18.4-trixie@sha256:3a82e1f56c8f0f5616a11103ac3d47e632c3938698946a7ad26da0df1334744a AS builder

ARG PGVECTOR_COMMIT
ARG PG_TEXTSEARCH_COMMIT

RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		build-essential \
		ca-certificates \
		git \
		postgresql-server-dev-18 \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /build

# OPTFLAGS="" drops -march=native so the artifact matches the build platform, not the build host CPU.
RUN git clone https://github.com/pgvector/pgvector.git pgvector \
	&& cd pgvector \
	&& git checkout --detach "${PGVECTOR_COMMIT}" \
	&& test "$(git rev-parse HEAD)" = "${PGVECTOR_COMMIT}" \
	&& make OPTFLAGS="" \
	&& make install

RUN git clone https://github.com/timescale/pg_textsearch.git pg_textsearch \
	&& cd pg_textsearch \
	&& git checkout --detach "${PG_TEXTSEARCH_COMMIT}" \
	&& test "$(git rev-parse HEAD)" = "${PG_TEXTSEARCH_COMMIT}" \
	&& make \
	&& make install

FROM postgres:18.4-trixie@sha256:3a82e1f56c8f0f5616a11103ac3d47e632c3938698946a7ad26da0df1334744a

COPY --from=builder /usr/lib/postgresql/18/lib/vector.so /usr/lib/postgresql/18/lib/
COPY --from=builder /usr/lib/postgresql/18/lib/pg_textsearch.so /usr/lib/postgresql/18/lib/
COPY --from=builder /usr/share/postgresql/18/extension/vector.control /usr/share/postgresql/18/extension/
COPY --from=builder /usr/share/postgresql/18/extension/vector--*.sql /usr/share/postgresql/18/extension/
COPY --from=builder /usr/share/postgresql/18/extension/pg_textsearch.control /usr/share/postgresql/18/extension/
COPY --from=builder /usr/share/postgresql/18/extension/pg_textsearch--*.sql /usr/share/postgresql/18/extension/
COPY --from=builder /build/pgvector/LICENSE /usr/share/doc/pgvector/LICENSE
COPY --from=builder /build/pg_textsearch/LICENSE /usr/share/doc/pg_textsearch/LICENSE
COPY --from=builder /build/pg_textsearch/NOTICE /usr/share/doc/pg_textsearch/NOTICE
