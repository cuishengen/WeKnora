# Compile artifacts/WeKnora from this fork with the upstream anydoc build tag.
# Keep the pinned upstream runtime's BrowserSkill, DuckDB, notices and tools.
ARG APP_BASE
FROM ${APP_BASE}
ARG SOURCE_COMMIT
LABEL org.opencontainers.image.source="https://github.com/cuishengen/WeKnora" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"
COPY artifacts/WeKnora /app/WeKnora
COPY migrations/ /app/migrations/
RUN chmod +x /app/WeKnora
