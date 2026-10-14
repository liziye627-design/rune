# A machine Rune reaches over ssh, running the real rune: the harness
# bind-mounts the binary at /opt/rune/rune, where ~/.local/bin/rune
# (install.sh's location) points.
#
# Every ssh session reports data races of the race-enabled binary under
# /tmp/race, which the harness fails the test on, and the coverage of a
# coverage build under /tmp/cover, where the harness mounts its own.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
		openssh-server procps \
	&& rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --shell /bin/sh test \
	&& usermod --password '*' test \
	&& mkdir -p /run/sshd /home/test/.ssh /home/test/.local/bin /tmp/race /tmp/cover \
	&& chmod 1777 /tmp/race /tmp/cover \
	&& ln -s /opt/rune/rune /home/test/.local/bin/rune \
	&& ssh-keygen -A \
	&& echo 'SetEnv GORACE=log_path=/tmp/race/race GOCOVERDIR=/tmp/cover' >> /etc/ssh/sshd_config
COPY workspacessh/test/testdata/id_ed25519.pub /home/test/.ssh/authorized_keys
RUN chown -R test:test /home/test \
	&& chmod 700 /home/test/.ssh \
	&& chmod 600 /home/test/.ssh/authorized_keys
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
