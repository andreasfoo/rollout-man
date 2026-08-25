"""A mock agent: instant "run", real trail.

The point is to exercise the pipeline fast -- pre hooks, admission, per-case
post, ship -- without waiting on an LLM. This agent finishes in the time it
takes to copy files:

  1. the deliverable goes into the container the same way oracle's does
     (upload the case's solution, run its solve.sh), so the verifier scores
     real container state and the reward means something;
  2. the packaged trail from the case's own jobs/<name>__*/agent/ lands in
     this trial's agent logs dir, so the artifacts that ship are the real
     recorded session, not something invented for the test.

Register with harbor as an import path (harbor only injects task_dir /
trial_paths into its own oracle, so we take task_dir as --ak and derive the
rest from logs_dir):
  -a mock_agent:MockAgent --ak task_dir=$CASE_DIR
(module found via PYTHONPATH=adapters; the harbor adapter sets it)

NB: it runs the solution, which is what oracle does -- so as a matrix agent
this mock is for plumbing tests, not for measuring anything. Its numbers are
oracle's numbers wearing a borrowed trail.
"""

from __future__ import annotations

import shutil
from pathlib import Path
from typing import override

from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext
from harbor.models.task.task import Task
from harbor.models.trial.paths import EnvironmentPaths, TrialPaths
from harbor.utils.scripts import build_execution_command, needs_chmod, quote_shell_arg


class MockAgent(BaseAgent):
    def __init__(
        self,
        logs_dir: Path,
        task_dir: Path,
        model_name: str | None = None,
        jobs_glob: str = "*",
        extra_env: dict[str, str] | None = None,
        agent_timeout_sec: float | None = None,
        **kwargs,
    ):
        super().__init__(
            logs_dir=logs_dir, model_name=model_name, extra_env=extra_env, **kwargs
        )
        self._task = Task(task_dir)
        # harbor hands agents logs_dir = <trial>/agent; the trial dir is where
        # TrialPaths lives. Rebuild enough of it to write exit-code/mock logs
        # where the rest of the machinery looks for them.
        self._trial_paths = TrialPaths(trial_dir=logs_dir.parent)
        self._jobs_glob = jobs_glob
        self._agent_timeout_sec = agent_timeout_sec

    @staticmethod
    @override
    def name() -> str:
        return "mock"

    @override
    def version(self) -> str:
        return "1.0.0"

    @override
    async def setup(self, environment: BaseEnvironment) -> None:
        return

    @override
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        # 1. The deliverable: exactly what oracle does, because the verifier
        #    reads container state and a trail without a score is decoration.
        env_paths = EnvironmentPaths.for_os(environment.os)
        solution_dir = self._task.paths.solution_dir
        solve_path = (
            self._task.paths.discovered_solve_path_for(
                self._task.config.environment.os
            )
            or self._task.paths.solve_path
        )
        if not solve_path.exists():
            raise FileNotFoundError(f"solution script not found: {solve_path}")

        await environment.upload_dir(
            source_dir=solution_dir,
            target_dir=str(env_paths.solution_dir),
        )
        container_solve = str(
            env_paths.solution_dir / solve_path.relative_to(solution_dir).as_posix()
        )
        if needs_chmod(container_solve):
            await environment.exec(
                command=f"chmod +x {quote_shell_arg(container_solve, environment.os)}",
                user="root",
            )
        timeout = int(self._agent_timeout_sec) if self._agent_timeout_sec else None
        mock_log = env_paths.agent_dir / "mock.txt"
        command = build_execution_command(
            container_solve, stdout_path=str(mock_log), task_os=environment.os
        )
        result = await environment.exec(
            command=command,
            env={"DEBIAN_FRONTEND": "noninteractive"},
            timeout_sec=timeout,
        )
        if not environment.capabilities.mounted:
            try:
                await environment.download_file(
                    source_path=str(mock_log),
                    target_path=self._trial_paths.agent_dir / "mock.txt",
                )
            except Exception as e:  # noqa: BLE001 - log, don't fail the trial
                self.logger.error(f"mock: download mock.txt failed: {e}")
        if result.return_code != 0:
            (self._trial_paths.agent_dir / "exit-code.txt").write_text(
                str(result.return_code)
            )

        # 2. The trail: the case's packaged session, copied in as this trial's
        #    agent output. First jobs/<case>__<suffix> that has an agent/ dir.
        jobs = Path(self._task.task_dir) / "jobs"
        agent_src = None
        if jobs.is_dir():
            for cand in sorted(jobs.glob(self._jobs_glob)):
                if (cand / "agent").is_dir() and any((cand / "agent").iterdir()):
                    agent_src = cand / "agent"
                    break
        if agent_src is None:
            self.logger.warning(
                f"mock: no packaged trail under {jobs}, shipping mock.txt only"
            )
            return
        dest = self._trial_paths.agent_dir
        dest.mkdir(parents=True, exist_ok=True)
        for item in agent_src.iterdir():
            target = dest / item.name
            if item.is_dir():
                if target.exists():
                    shutil.rmtree(target)
                shutil.copytree(item, target)
            else:
                shutil.copy2(item, target)
        self.logger.info(f"mock: trail copied from {agent_src}")
