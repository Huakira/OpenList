FROM python:3.13-slim
WORKDIR /app
COPY pyproject.toml ./
COPY icloud_bridge ./icloud_bridge
RUN pip install --no-cache-dir .
ENV BRIDGE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=60s --timeout=10s CMD python -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:8080/v1/health')"
ENTRYPOINT ["icloud-bridge"]
CMD ["serve"]
