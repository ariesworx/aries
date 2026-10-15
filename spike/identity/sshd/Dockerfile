# sshd as the server guide sets it up for the aries account, to check
# what it does with expiry-time (RESULTS.md, section 4). BASE is an
# Ubuntu image pinned by digest; run.sh passes 24.04's and 26.04's.
ARG BASE
FROM ${BASE}
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --home-dir /var/lib/aries --create-home --shell /bin/sh aries \
 && install -d -o aries -g aries -m 0700 /var/lib/aries/.ssh \
 && install -d -m 0755 /run/sshd \
 && ssh-keygen -A
COPY aries.conf /etc/ssh/sshd_config.d/aries.conf
COPY aries-probe /usr/local/bin/aries-probe
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/aries-probe /usr/local/bin/entrypoint.sh \
 && ln -s aries-probe /usr/local/bin/ariesd
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
