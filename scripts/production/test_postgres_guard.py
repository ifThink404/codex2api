import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class PostgresGuardTests(unittest.TestCase):
    def run_guard(self, candidate=None, status="running", ready=True):
        current = {
            "DATABASE_DRIVER": "postgres",
            "DATABASE_HOST": "codex2api-postgres",
            "DATABASE_PORT": "5432",
            "DATABASE_USER": "codex2api",
            "DATABASE_PASSWORD": "secret-not-to-log",
            "DATABASE_NAME": "codex2api",
            "DATABASE_SSLMODE": "disable",
        }
        updated = dict(current)
        updated.update(candidate or {})
        fixture = {
            "current": [f"{key}={value}" for key, value in current.items()],
            "candidate": [f"{key}={value}" for key, value in updated.items()],
        }
        with tempfile.TemporaryDirectory() as directory:
            docker = Path(directory) / "docker"
            docker.write_text("""#!/usr/bin/env python3
import json, os, sys
if sys.argv[1] == 'inspect':
    if sys.argv[2] == 'codex2api-postgres':
        print(os.environ['PG_STATUS'])
    else:
        print(json.dumps(json.loads(os.environ['FIXTURE'])[sys.argv[2]]))
elif sys.argv[1] == 'exec':
    if os.environ['PG_READY'] != '1':
        sys.exit(1)
    if 'psql' in sys.argv:
        print('1')
else:
    sys.exit(2)
""")
            docker.chmod(0o755)
            environment = dict(os.environ)
            environment.update(
                PATH=directory + os.pathsep + os.environ["PATH"],
                FIXTURE=json.dumps(fixture),
                PG_STATUS=status,
                PG_READY="1" if ready else "0",
            )
            result = subprocess.run(
                ["bash", str(Path(__file__).with_name("verify-postgres.sh")),
                 "current", "candidate"],
                env=environment, text=True, capture_output=True,
            )
            self.assertNotIn("secret-not-to-log", result.stdout + result.stderr)
            return result

    def test_matching_postgres_is_allowed(self):
        self.assertEqual(self.run_guard().returncode, 0)

    def test_sqlite_is_rejected(self):
        self.assertNotEqual(self.run_guard({"DATABASE_DRIVER": "sqlite"}).returncode, 0)

    def test_missing_host_is_rejected(self):
        self.assertNotEqual(self.run_guard({"DATABASE_HOST": ""}).returncode, 0)

    def test_different_database_is_rejected(self):
        self.assertNotEqual(self.run_guard({"DATABASE_NAME": "empty"}).returncode, 0)

    def test_different_password_is_rejected(self):
        self.assertNotEqual(self.run_guard({"DATABASE_PASSWORD": "wrong"}).returncode, 0)

    def test_stopped_postgres_is_rejected(self):
        self.assertNotEqual(self.run_guard(status="exited").returncode, 0)

    def test_unready_postgres_is_rejected(self):
        self.assertNotEqual(self.run_guard(ready=False).returncode, 0)


if __name__ == "__main__":
    unittest.main()
