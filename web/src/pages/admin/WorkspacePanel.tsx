import { useState, type FormEvent } from "react";
import type { AgentSettings } from "../../api/admin";
import { AGENT_LABEL } from "../../api/agentText";
import type { AgentType } from "../../api/agents";
import ErrorMessage, { describeError } from "../../components/ErrorMessage";
import { useBootstrapRules, useUpdateSettings, useWorkspaceSettings } from "../../hooks/useAdmin";
import { useWorkspaces } from "../../hooks/useWorkspaces";

const AGENTS = Object.keys(AGENT_LABEL) as AgentType[];

export default function WorkspacePanel() {
  const workspaces = useWorkspaces();
  const [chosen, setChosen] = useState<string | null>(null);
  const list = workspaces.data ?? [];
  const id = chosen ?? list[0]?.id ?? null;

  return (
    <section aria-label="Workspace settings">
      <h2>Workspaces</h2>
      {workspaces.isPending && <p role="status">Loading workspaces...</p>}
      {workspaces.isError && <ErrorMessage error={workspaces.error} onRetry={() => void workspaces.refetch()} />}
      {workspaces.data && list.length === 0 && <p>This tenant has no workspace yet.</p>}
      {list.length > 0 && (
        <label>
          Workspace{" "}
          <select value={id ?? ""} onChange={(e) => setChosen(e.target.value)}>
            {list.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {id && <Settings key={id} id={id} />}
    </section>
  );
}

function Settings({ id }: { id: string }) {
  const q = useWorkspaceSettings(id);
  if (q.isPending) return <p role="status">Loading settings...</p>;
  if (q.isError) return <ErrorMessage error={q.error} onRetry={() => void q.refetch()} />;
  return (
    <>
      <SettingsForm id={id} settings={q.data.agent_settings} />
      <Rules id={id} />
    </>
  );
}

function SettingsForm({ id, settings }: { id: string; settings: AgentSettings }) {
  const update = useUpdateSettings(id);
  const [enabled, setEnabled] = useState(settings.agents_enabled);
  const [hitl, setHitl] = useState(settings.require_hitl_for_all);
  const [max, setMax] = useState(String(settings.max_subtasks_per_decomposition));
  const [off, setOff] = useState<string[]>(settings.disabled_agents);
  const [saved, setSaved] = useState(false);

  function submit(e: FormEvent) {
    e.preventDefault();
    setSaved(false);
    update.mutate(
      {
        agents_enabled: enabled,
        require_hitl_for_all: hitl,
        max_subtasks_per_decomposition: Number(max),
        disabled_agents: off as AgentType[],
      },
      { onSuccess: () => setSaved(true) },
    );
  }

  return (
    <form onSubmit={submit} className="edit" aria-label="Agent settings">
      <label>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} /> Agents enabled
      </label>
      <label>
        <input type="checkbox" checked={hitl} onChange={(e) => setHitl(e.target.checked)} /> Require approval for every agent change
      </label>
      <label>
        Most subtasks per decomposition
        <input type="number" min={1} value={max} onChange={(e) => setMax(e.target.value)} />
      </label>
      <fieldset>
        <legend>Turned off agents</legend>
        {AGENTS.map((a) => (
          <label key={a}>
            <input type="checkbox" checked={off.includes(a)} onChange={(e) => setOff(e.target.checked ? [...off, a] : off.filter((x) => x !== a))} /> {AGENT_LABEL[a]}
          </label>
        ))}
      </fieldset>
      <div className="actions">
        <button type="submit" disabled={update.isPending}>
          Save settings
        </button>
      </div>
      {saved && <p role="status">Settings saved.</p>}
      {update.isError && (
        <p role="alert" className="error">
          {describeError(update.error)}
        </p>
      )}
    </form>
  );
}

function Rules({ id }: { id: string }) {
  const boot = useBootstrapRules(id);
  return (
    <div>
      <h3>Notification rules</h3>
      <p className="meta">Registers the default rules at AgentPulse. Rules already registered are not sent again.</p>
      <button type="button" disabled={boot.isPending} onClick={() => boot.mutate()}>
        Bootstrap rules
      </button>
      {boot.isSuccess && (
        <p role="status">
          {boot.data.length} {boot.data.length === 1 ? "rule is" : "rules are"} registered.
        </p>
      )}
      {boot.isError && (
        <p role="alert" className="error">
          {describeError(boot.error)}
        </p>
      )}
    </div>
  );
}
