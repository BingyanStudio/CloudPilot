#!/usr/bin/env python3
"""Run an opt-in HTTP → Novita fixture check, with a draft PR or local-only fix."""

import argparse
import base64
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import socket
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


PROVIDER = "https://api.us-phx-1.sandbox.novita.ai"
TERMINAL = {"succeeded", "failed", "cancelled", "interrupted"}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("binary", "key-file", "template", "repository", "base-ref", "head-ref", "output-dir"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--github-token-file", help="Defaults to the GH_TOKEN environment variable")
    parser.add_argument("--no-pr", action="store_true", help="Fix and test a public repository locally; do not push or create a PR")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repository):
        parser.error("--repository must be owner/name")
    for value in (args.base_ref, args.head_ref):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/-]*", value) or any(
            part in value for part in ("..", "//", ".lock")
        ) or value.endswith(("/", ".")):
            parser.error("base/head refs must be plain Git branch names or commit IDs")
    if args.base_ref == args.head_ref:
        parser.error("head ref must differ from the isolated baseline ref")
    return args


class Check:
    def __init__(self, args):
        self.args = args
        self.output = Path(args.output_dir).resolve()
        self.output.mkdir(parents=True, exist_ok=True, mode=0o700)
        if (self.output / "report.json").exists() or (self.output / "cloudpilot.db").exists():
            raise RuntimeError("output directory contains a previous run; choose a new directory")
        self.key = Path(args.key_file).read_text().strip()
        self.github = "" if args.no_pr else (Path(args.github_token_file).read_text().strip()
                                            if args.github_token_file else os.environ.get("GH_TOKEN", ""))
        if not self.key:
            raise RuntimeError("Novita key file must not be empty")
        if not args.no_pr and not self.github:
            raise RuntimeError("GH_TOKEN (or --github-token-file) is required for PR mode")
        self.password, self.webhook = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
        self.encryption_key = base64.b64encode(secrets.token_bytes(32)).decode()
        self.hidden = sorted({value for value in (self.key, self.github, self.password, self.webhook, self.encryption_key) if value}, key=len, reverse=True)
        self.local = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect(),
        )
        self.remote = urllib.request.build_opener(NoRedirect())
        self.process = None
        self.reader = None
        self.run_id = None
        self.base_url = ""
        self.report = {"repository": args.repository, "baseRef": args.base_ref,
                       "headRef": args.head_ref, "template": args.template,
                       "prRequested": not args.no_pr, "pullRequests": [],
                       "validationScope": "execution-and-artifact-collection",
                       "behavior": "Public repository clone, local branch fix, tests and artifacts only; no push or PR requested."
                       if args.no_pr else "Isolated fixture fix, tests, unique branch push and draft PR requested; no merge.",
                       "startedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                       "success": False, "cleanup": {"errors": [], "fallbackDeleted": []}}

    def redact(self, text):
        for value in self.hidden:
            text = text.replace(value, "[REDACTED]")
        return text

    def save(self, name, value):
        data = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, indent=2) + "\n"
        path = self.output / name
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with path.open("w", encoding="utf-8") as stream:
            os.chmod(path, 0o600)
            stream.write(self.redact(data))

    def request(self, method, path, body=None, headers=None, expected=(200,), binary=False):
        request_headers = dict(headers or {})
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            request_headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.base_url + path, data=data, headers=request_headers, method=method)
        try:
            with self.local.open(request, timeout=30) as response:
                raw, status = response.read(), response.status
        except urllib.error.HTTPError as error:
            raise RuntimeError(f"{method} {path}: HTTP {error.code}: "
                               + self.redact(error.read(4096).decode(errors="replace"))) from None
        if status not in expected:
            raise RuntimeError(f"{method} {path}: expected {expected}, received {status}")
        return raw if binary else json.loads(raw) if raw else None

    def provider(self, method, path):
        request = urllib.request.Request(PROVIDER + path, headers={"X-API-KEY": self.key}, method=method)
        try:
            with self.remote.open(request, timeout=30) as response:
                raw = response.read()
                return response.status, json.loads(raw) if raw else None
        except urllib.error.HTTPError as error:
            if error.code == 404:
                error.close()
                return 404, None
            raise RuntimeError(f"Novita {method} {path}: HTTP {error.code}") from None

    def start(self):
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        self.base_url = f"http://127.0.0.1:{port}"
        config = {"server": {"address": f"127.0.0.1:{port}", "secure_cookies": False},
                  "database": {"driver": "sqlite", "dsn": str(self.output / "cloudpilot.db")},
                  "admin": {"username": "e2e-admin"},
                  "runtime": {"data_dir": str(self.output / "data")},
                  "scheduler": {"poll_interval": "500ms"}, "metrics": {"address": ""}}
        self.save("config.yaml", config)  # JSON is also valid YAML.
        env = {key: value for key, value in os.environ.items() if not key.startswith("CLOUDPILOT_")}
        env.update(CLOUDPILOT_ADMIN_PASSWORD=self.password, CLOUDPILOT_ENCRYPTION_KEY=self.encryption_key)
        if self.args.no_pr:
            env.pop("GH_TOKEN", None)
            env.pop("GITHUB_TOKEN", None)
        else:
            env["GH_TOKEN"] = self.github
        self.process = subprocess.Popen([str(Path(self.args.binary).resolve()), "-config", str(self.output / "config.yaml")],
                                        env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                        text=True, errors="replace")

        def capture():
            with (self.output / "server.log").open("w", encoding="utf-8") as stream:
                os.chmod(self.output / "server.log", 0o600)
                for line in self.process.stdout:
                    stream.write(self.redact(line))
                    stream.flush()

        self.reader = threading.Thread(target=capture, daemon=True)
        self.reader.start()
        print(f"Starting isolated CloudPilot at {self.base_url}", flush=True)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError("CloudPilot exited during startup; see server.log")
            try:
                self.request("GET", "/health")
                break
            except (OSError, RuntimeError):
                time.sleep(0.25)
        else:
            raise RuntimeError("CloudPilot did not become healthy within 30 seconds")
        self.request("POST", "/api/auth/login", {"username": "e2e-admin", "password": self.password})

    def import_resources(self):
        args = self.args
        instructions = f"""This is an authorized, isolated Novita sandbox end-to-end test in {args.repository}.
Work only in /workspace/project/e2e/cart-demo. Read its README and preserve all existing tests.
First run npm test in that directory and capture the failing baseline and exit code in /output/baseline-tests.txt.
Fix the demonstrated cart defects according to the README, then run npm test again and save the full result in /output/final-tests.txt.
Only change repository files beneath e2e/cart-demo/. Do not weaken or delete tests or change package scripts to hide failures.
Create a new local branch {args.head_ref} from the checked-out baseline {args.base_ref}; do not reset any existing branch.
Save the fix patch against the baseline in /output/fix.patch. Keep logs and generated artifacts outside the repository.
Write /output/artifacts.json containing baseline-tests.txt, final-tests.txt and fix.patch as kind file,
and a kind text result explaining the reproduced failure and successful checks. Never fabricate a test result.
Stop and report an error if permission or test requirements fail.
"""
        if args.no_pr:
            instructions += "Do not push, create a PR or issue, comment, merge, or make any remote changes. This is a local-fix-only test without GitHub credentials. Do not include a PR artifact.\n"
        else:
            instructions += f"""Commit the fixture fix, push only {args.head_ref}, then use gh pr create --draft --repo {args.repository} --base {args.base_ref} --head {args.head_ref}.
The PR title/body must identify this as an isolated Novita E2E test and describe the verified fix and test results.
Do not force-push, merge, close, comment on, or otherwise change any other PR or branch. Do not create issues.
Include the real PR URL as a kind pr entry in /output/artifacts.json. Never fabricate a PR URL.
Use the provided GH_TOKEN without printing it or writing it into files.
"""
        specs = [
            ("Worker", "e2e-worker", {"type": "codex", "model": "deepseek/deepseek-v4.1-flash-1017",
             "endpoint": "https://api.novita.ai/openai/v1", "apiKey": self.key, "instructions": instructions}),
            ("Workspace", "e2e-workspace", {"repositories": [{"url": f"https://github.com/{args.repository}.git",
             "ref": args.base_ref, "path": "project"}], "prompt": instructions, "tools": [], "mcpServers": {},
             "env": {"GIT_AUTHOR_NAME": "CloudPilot E2E", "GIT_AUTHOR_EMAIL": "cloudpilot@users.noreply.github.com",
                     "GIT_COMMITTER_NAME": "CloudPilot E2E", "GIT_COMMITTER_EMAIL": "cloudpilot@users.noreply.github.com"},
             "secretEnv": {} if args.no_pr else {"GH_TOKEN": "GH_TOKEN"}}),
            ("Runner", "e2e-runner", {"type": "e2b", "e2b": {"provider": "novita",
             "domain": "us-phx-1.sandbox.novita.ai", "apiUrl": PROVIDER, "apiKey": self.key,
             "template": args.template, "leaseSeconds": 300, "idleStrategy": "pause"}}),
            ("Pipeline", "e2e-pipeline", {"workerRef": "e2e-worker", "workspaceRef": "e2e-workspace",
             "runnerRef": "e2e-runner", "schedule": "", "timezone": "Asia/Shanghai", "concurrency": 1,
             "paused": False, "maxRetries": 0, "timeoutSeconds": 600}),
            ("Input", "e2e-feedback", {"type": "webhook", "pipelineRef": "e2e-pipeline",
             "secret": self.webhook, "paused": False}),
        ]
        documents = [{"apiVersion": "cloudpilot.dev/v1alpha1", "kind": kind, "metadata": {"name": name}, "spec": spec}
                     for kind, name, spec in specs]
        manifest = "\n---\n".join(json.dumps(doc, ensure_ascii=False) for doc in documents)
        self.save("resources.yaml", manifest)
        imported = self.request("POST", "/api/config/import", {"yaml": manifest})
        if len(imported) != 5:
            raise RuntimeError("resource import did not return all five resources")
        print("Imported five resources; submitting the real webhook", flush=True)

    def execute(self):
        feedback = "The isolated cart demo has known defects. Reproduce npm test failures and fix them according to its README. "
        feedback += "Keep the fix local and publish test logs and the patch; make no remote changes." if self.args.no_pr else "Create the authorized draft PR."
        payload = {"feedback": feedback,
                   "repository": self.args.repository, "baseRef": self.args.base_ref, "headRef": self.args.head_ref}
        headers = {"Authorization": "Bearer " + self.webhook, "Idempotency-Key": "novita-e2e-" + secrets.token_hex(12)}
        event = self.request("POST", "/hooks/e2e-feedback", payload, headers, expected=(202,))
        self.report["eventId"] = event["id"]
        self.save("event.json", event)
        duplicate = self.request("POST", "/hooks/e2e-feedback", payload, headers, expected=(202,))
        if event["id"] != duplicate["id"]:
            raise RuntimeError("duplicate webhook created a different event")
        self.report.update(eventId=event["id"], webhookDeduplicated=True)
        deadline, previous = time.monotonic() + 600, None
        while time.monotonic() < deadline:
            runs = [run for run in self.request("GET", "/api/runs") if run["eventId"] == event["id"]]
            if len(runs) > 1:
                raise RuntimeError("the deduplicated event unexpectedly produced multiple runs")
            state = "queued"
            if runs:
                self.run_id = runs[0]["id"]
                self.report["runId"] = self.run_id
                detail = self.request("GET", f"/api/runs/{self.run_id}")
                state = detail["run"]["status"]
                if state in TERMINAL:
                    self.save("run.json", detail)
                    if state != "succeeded":
                        raise RuntimeError(f"Run {self.run_id} {state}: {detail['run'].get('error', '')}")
                    if not detail.get("session") or detail["session"]["state"] != "idle":
                        raise RuntimeError("successful run did not retain an idle interactive session")
                    self.collect(detail)
                    outcome = "local fix, test logs and patch collected" if self.args.no_pr else "PR artifact collected"
                    print(f"Run {self.run_id}: succeeded; {outcome}", flush=True)
                    return
            if state != previous:
                print(f"Event {event['id']}, run {self.run_id or '-'}: {state}", flush=True)
                previous = state
            if self.process.poll() is not None:
                raise RuntimeError("CloudPilot exited while the event was running")
            time.sleep(2)
        raise RuntimeError("end-to-end execution exceeded ten minutes")

    def pages(self, kind):
        result, after = [], 0
        while True:
            page = self.request("GET", f"/api/runs/{self.run_id}/{kind}?after={after}")
            result.extend(page)
            if len(page) < 1000:
                return result
            after = page[-1]["id"]

    def collect(self, detail=None):
        if self.run_id is None:
            return
        detail = detail or self.request("GET", f"/api/runs/{self.run_id}")
        self.save("run.json", detail)
        self.save("timeline.json", self.pages("timeline"))
        self.save("logs.json", self.pages("logs"))
        artifacts = detail.get("artifacts", [])
        self.save("artifacts.json", artifacts)
        urls = []
        downloaded = []
        for artifact in artifacts:
            if artifact["kind"] == "pr":
                if self.args.no_pr:
                    raise RuntimeError("local-fix-only execution unexpectedly published a PR artifact")
                parsed = urllib.parse.urlsplit(artifact["url"])
                pattern = "/" + re.escape(self.args.repository) + r"/pull/[1-9][0-9]*"
                if parsed.scheme != "https" or parsed.netloc != "github.com" or not re.fullmatch(pattern, parsed.path) or parsed.query or parsed.fragment:
                    raise RuntimeError("PR artifact does not identify a pull request in the authorized repository")
                urls.append(artifact["url"])
            if artifact["kind"] == "file":
                data = self.request("GET", f"/api/artifacts/{artifact['id']}/download", binary=True)
                name = re.sub(r"[^A-Za-z0-9_.-]+", "_", artifact["name"]).strip(".") or "artifact"
                filename = f"downloads/{artifact['id']}-{name}"
                self.save(filename, data.decode("utf-8", errors="replace"))
                downloaded.append({"id": artifact["id"], "name": artifact["name"], "path": filename})
        self.report.update(pullRequests=urls, downloads=downloaded)
        expected_prs = 0 if self.args.no_pr else 1
        if detail["run"]["status"] == "succeeded" and (len(urls) != expected_prs or len(downloaded) < 3):
            raise RuntimeError(f"successful execution must publish {expected_prs} PR artifact(s) and baseline/final test logs plus a patch")

    def instances(self):
        database = self.output / "cloudpilot.db"
        if not database.exists():
            return []
        with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as db:
            db.row_factory = sqlite3.Row
            return [dict(row) for row in db.execute(
                "SELECT id, deployment_id, event_id, run_id, attempt_id, sandbox_id, state, last_error FROM runtime_instances"
            )]

    def cleanup(self):
        errors = self.report["cleanup"]["errors"]
        if not self.run_id and self.report.get("eventId") and self.process and self.process.poll() is None:
            try:
                runs = [run for run in self.request("GET", "/api/runs") if run["eventId"] == self.report["eventId"]]
                if runs:
                    self.run_id = runs[0]["id"]
                    self.report["runId"] = self.run_id
            except Exception as error:
                errors.append(self.redact(str(error)))
        if self.run_id and self.process and self.process.poll() is None:
            try:
                self.collect()
            except Exception as error:
                errors.append(self.redact(str(error)))
            try:
                self.request("POST", f"/api/runs/{self.run_id}/close", expected=(202,))
                deadline = time.monotonic() + 75
                while time.monotonic() < deadline:
                    detail = self.request("GET", f"/api/runs/{self.run_id}")
                    if detail.get("session", {}).get("state") == "closed":
                        self.report["cleanup"]["sessionClosed"] = True
                        self.save("closed-run.json", detail)
                        break
                    time.sleep(2)
                else:
                    raise RuntimeError("session close was not confirmed within 75 seconds")
            except Exception as error:
                errors.append(self.redact(str(error)))
        if errors or (not self.run_id and self.report.get("eventId")):
            self.stop()  # Stop further execution before emergency cleanup of this test's resources.
        try:
            records = self.instances()
            self.save("runtime-instances.json", records)
            checks = []
            for record in records:
                if record["state"] != "closed":
                    errors.append(f"runtime instance {record['id']} remains {record['state']}")
                candidates = [record["sandbox_id"]] if record["sandbox_id"] else []
                if not candidates and record["state"] not in ("closed", "lost"):
                    metadata = urllib.parse.urlencode({"cloudpilot_instance": record["deployment_id"], "cloudpilot_attempt": record["attempt_id"]})
                    query = urllib.parse.urlencode({"state": "running,paused", "metadata": metadata, "limit": 100})
                    _, matches = self.provider("GET", "/v2/sandboxes?" + query)
                    candidates = [item["sandboxID"] for item in matches]
                for sandbox_id in candidates:
                    path = "/sandboxes/" + urllib.parse.quote(sandbox_id, safe="")
                    status, info = self.provider("GET", path)
                    checks.append({"sandboxId": sandbox_id, "initialStatus": status})
                    if status != 404:
                        metadata = info.get("metadata", {})
                        if metadata.get("cloudpilot_instance") != record["deployment_id"] or metadata.get("cloudpilot_attempt") != record["attempt_id"]:
                            raise RuntimeError("refusing fallback deletion: sandbox ownership does not match this test")
                        errors.append(f"runtime did not delete test sandbox {sandbox_id}; fallback deletion required")
                        self.provider("DELETE", path)
                        self.report["cleanup"]["fallbackDeleted"].append(sandbox_id)
                        status, _ = self.provider("GET", path)
                    checks[-1]["finalStatus"] = status
                    if status != 404:
                        raise RuntimeError(f"test sandbox {sandbox_id} deletion was not confirmed")
            self.report["cleanup"]["sandboxes"] = checks
            if self.report["success"] and not records:
                errors.append("successful execution has no persisted sandbox instance")
        except Exception as error:
            errors.append(self.redact(str(error)))

    def stop(self):
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=45)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
                self.report["cleanup"]["errors"].append("CloudPilot required SIGKILL after shutdown timeout")
        if self.reader:
            self.reader.join(timeout=5)

    def run(self):
        try:
            self.start()
            self.import_resources()
            self.execute()
            self.report["success"] = True
        except (Exception, KeyboardInterrupt) as error:
            self.report["error"] = self.redact(str(error) or type(error).__name__)
        finally:
            self.cleanup()
            self.stop()
            if self.report["cleanup"]["errors"]:
                self.report["success"] = False
            self.report["finishedAt"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            self.save("report.json", self.report)
        print(self.redact(json.dumps(self.report, ensure_ascii=False, indent=2)), flush=True)
        return 0 if self.report["success"] else 1


def main():
    os.umask(0o077)
    try:
        return Check(arguments()).run()
    except Exception as error:
        print(f"E2E setup failed: {type(error).__name__}: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
