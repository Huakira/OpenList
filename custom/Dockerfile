# Official all-in-one image with only the openlist binary swapped for the custom build
# (upstream + drivers icloud_photos and pixcake). See custom/build.sh.
ARG OPENLIST_VERSION=v4.2.9
FROM openlistteam/openlist:${OPENLIST_VERSION}-aio
COPY --chown=1001:1001 --chmod=755 openlist /opt/openlist/openlist
