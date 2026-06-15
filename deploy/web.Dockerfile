# syntax=docker/dockerfile:1.7

FROM node:20-bookworm-slim AS build
WORKDIR /src/apps/web
COPY apps/web/package.json apps/web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --prefer-offline --no-audit || npm install --prefer-offline --no-audit
COPY apps/web ./
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /src/apps/web/dist /usr/share/nginx/html
COPY deploy/nginx/web.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
