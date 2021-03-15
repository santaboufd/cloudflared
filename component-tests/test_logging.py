#!/usr/bin/env python
import json
import os
from util import start_cloudflared, wait_tunnel_ready, send_requests, LOGGER


class TestLogging:
    # TODO: Test logging when running as a service https://jira.cfops.it/browse/TUN-4082
    # Rolling logger rotate log files after 1 MB
    rotate_after_size = 1000 * 1000
    default_log_file = "cloudflared.log"
    expect_message = "Starting tunnel"

    def test_logging_to_terminal(self, tmp_path, component_tests_config):
        config = component_tests_config()
        with start_cloudflared(tmp_path, config, new_process=True) as cloudflared:
            wait_tunnel_ready(tunnel_url=config.get_url())
            self.assert_log_to_terminal(cloudflared)

    def test_logging_to_file(self, tmp_path, component_tests_config):
        log_file = tmp_path / self.default_log_file
        extra_config = {
            # Convert from pathlib.Path to str
            "logfile": str(log_file),
        }
        config = component_tests_config(extra_config)
        with start_cloudflared(tmp_path, config, new_process=True, capture_output=False):
            wait_tunnel_ready(tunnel_url=config.get_url())
            self.assert_log_in_file(log_file)
            self.assert_json_log(log_file)

    def test_logging_to_dir(self, tmp_path, component_tests_config):
        log_dir = tmp_path / "logs"
        extra_config = {
            "loglevel": "debug",
            # Convert from pathlib.Path to str
            "log-directory": str(log_dir),
        }
        config = component_tests_config(extra_config)
        with start_cloudflared(tmp_path, config, new_process=True, capture_output=False):
            wait_tunnel_ready(tunnel_url=config.get_url())
            self.assert_log_to_dir(config, log_dir)

    def assert_log_to_terminal(self, cloudflared):
        stderr = cloudflared.stderr.read(200)
        # cloudflared logs the following when it first starts
        # 2021-03-10T12:30:39Z INF Starting tunnel tunnelID=<tunnel ID>
        assert self.expect_message.encode(
        ) in stderr, f"{stderr} doesn't contain {self.expect_message}"

    def assert_log_in_file(self, file, expect_message=""):
        with open(file, "r") as f:
            log = f.read(200)
            # cloudflared logs the following when it first starts
            # {"level":"info","tunnelID":"<tunnel ID>","time":"2021-03-10T12:21:13Z","message":"Starting tunnel"}
            assert self.expect_message in log, f"{log} doesn't contain {self.expect_message}"

    def assert_json_log(self, file):
        with open(file, "r") as f:
            line = f.readline()
            json_log = json.loads(line)
            self.assert_in_json(json_log, "level")
            self.assert_in_json(json_log, "time")
            self.assert_in_json(json_log, "message")

    def assert_in_json(self, j, key):
        assert key in j, f"{key} is not in j"

    def assert_log_to_dir(self, config, log_dir):
        max_batches = 3
        batch_requests = 1000
        for _ in range(max_batches):
            send_requests(config.get_url(),
                          batch_requests, require_ok=False)
            files = os.listdir(log_dir)
            if len(files) == 2:
                current_log_file_index = files.index(self.default_log_file)
                current_file = log_dir / files[current_log_file_index]
                stats = os.stat(current_file)
                assert stats.st_size > 0
                self.assert_json_log(current_file)

                # One file is the current log file, the other is the rotated log file
                rotated_log_file_index = 0 if current_log_file_index == 1 else 1
                rotated_file = log_dir / files[rotated_log_file_index]
                stats = os.stat(rotated_file)
                assert stats.st_size > self.rotate_after_size
                self.assert_log_in_file(rotated_file)
                self.assert_json_log(current_file)
                return

        raise Exception(
            f"Log file isn't rotated after sending {max_batches * batch_requests} requests")
