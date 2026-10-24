FROM node:24-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205
WORKDIR /opt/workspace
COPY gateway.mjs index.html ui.js ./
USER node
EXPOSE 18787
ENTRYPOINT ["node", "/opt/workspace/gateway.mjs"]
