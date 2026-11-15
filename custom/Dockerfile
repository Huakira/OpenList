# Official all-in-one image, plus a Python runtime for the embedded iCloud
# bridge, with only the openlist binary swapped for the custom build.
# The bridge's own source ships inside the binary; this file installs its
# third-party dependencies (requirements.txt).
ARG OPENLIST_VERSION=v4.2.9
FROM openlistteam/openlist:${OPENLIST_VERSION}-aio

USER root
COPY requirements.txt /tmp/requirements.txt
RUN apk add --no-cache python3 py3-pip \
 && apk add --no-cache --virtual .pybuild gcc musl-dev python3-dev libffi-dev openssl-dev \
 && pip install --no-cache-dir --break-system-packages --root-user-action=ignore -r /tmp/requirements.txt \
 && apk del .pybuild \
 && rm -f /tmp/requirements.txt

COPY --chown=1001:1001 --chmod=755 openlist /opt/openlist/openlist
