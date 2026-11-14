# gar (Go And Run)

O agente registra o comando, o humano roda no terminal dele e o agente lê o resultado.
Serve para quando o agente não pode rodar sozinho: o guard bloqueia `ssh`, ou o comando pede senha.

## Fluxo

1. O agente registra os passos:
   ```sh
   gar add --why "atualizar o macbot" -- \
     'ssh macbot "cd ~/Sites/dotfiles-2026 && git pull --ff-only origin main && ./switch"' \
     'bora --version'
   # caso 1020 registrado em bora:w1D6
   # [HUMAN] ! gar run
   ```
2. O agente deixa `gar wait 1020` rodando em background.
3. O humano roda `! gar run` no mesmo workspace: aparecem os casos pendentes dali, o mais antigo
   marcado. `↑`/`↓` escolhe, `Enter` roda, `Esc` sai sem mexer em nada. Cada passo roda num PTY
   (`$SHELL -c`), então prompt de `sudo`/ssh e Ctrl-C funcionam. Para no primeiro que falhar.
4. O `gar wait` do agente sai com o registro em JSON: saída e exit code de cada passo.

### Workspace

O `gar add` grava em `workspace` onde o agente estava; o `gar run` sem id usa a mesma detecção:

| Onde | Detecta por | Valor |
|---|---|---|
| `$GAR_WORKSPACE` definido | a própria variável | o valor dela |
| bora / herdr TUI | `$HERDR_WORKSPACE_ID` (+ nome do binário em `$HERDR_BIN_PATH`) | `bora:w1D6` |
| Herdr app | `$HC_TERMINAL_ID` → espaço do terminal em `hc ls --json` | `herdr-app:<espaço>` (sem resposta do `hc`: `herdr-app:terminal:<id>`) |
| nenhum | — | vazio: `add` imprime `! gar run <id>`, e `run` sem id oferece todos os pendentes |

## Comandos

| Comando | O que faz |
|---|---|
| `gar add --why s [--expect s] [--from s] [--timeout dur] [--parallel] [--json] -- <cmd>...` | Registra um caso; cada argumento depois de `--` é um passo. `--why` é obrigatório e diz a barreira que impede o agente de rodar sozinho (hook, EPERM, TTY, sudo/senha, login); ausente ou só espaços, sai com 64 sem gravar nada. `--from` padrão: `$GAR_FROM`. `--timeout` limita cada passo (padrão `10m`): estourou, o passo fica `failed` com `reason: "timeout"` e exit 124, os seguintes são pulados e o `wait` volta. Casos antigos sem o campo usam o padrão. `--parallel` marca o caso como paralelo (`"parallel": true`): o `run` dele não entra na fila e roda junto de qualquer outro. |
| `gar run [<id>] [--yes] [--again]` | Só humano: exige TTY em stdin e stdout. Sem id: seletor com os pendentes do workspace, mais antigo primeiro; `Enter` já confirma. Com id: mostra os passos e pergunta `[y/N]` (`--yes` pula). `--again` roda de novo um caso terminado. Responder não cancela o caso. Casos sequenciais (o padrão) rodam um por vez: depois da escolha/confirmação o `run` pega o flock de `$GAR_HOME/run.lock`; ocupado, imprime `gar: na fila atrás do caso <id> (há <t>, pid <p>); espera ele terminar (Ctrl-C desiste)` e espera. Seletor e `[y/N]` não seguram a fila. Ctrl-C na fila cancela o caso (fica `cancelled`, exit 130), a menos que outro `run` já o tenha pego. Caso já `running` é recusado antes da fila (exit 4); dois `run` esperando o mesmo caso: o segundo, ao sair da fila, diz que o caso já rodou (exit 4). Casos `--parallel` não entram na fila. |
| `gar wait [<id>] [--timeout dur]` | Espera o caso terminar (poll de 500 ms) e imprime o JSON. Sem timeout padrão. Sem id: espera os casos `running` agora no workspace (o mesmo escopo do seletor do `run`; sem workspace, todos). Um caso: saída e exit iguais a `gar wait <id>`. Vários: array JSON, exit 0 só se todos `done`, senão o exit do primeiro que não terminou `done` (124 se ainda não terminou no timeout). Nenhum `running`: exit 4. |
| `gar show <id> [--json]` | Resumo: passo, exit, duração, últimas linhas da saída. |
| `gar ls [--status s] [--limit n]` | Lista os casos, mais novos primeiro (padrão 20). |
| `gar cancel <id>` | Cancela um caso `pending`. |
| `gar sql "<query>"` | Opcional: precisa do `duckdb` no PATH. Views `runs` e `steps` (um passo por linha, com `run_id`). |

## Exit codes

| Código | Quando |
|---|---|
| 0 | ok; `wait` com caso `done` |
| 1 | `wait` com caso `failed`; erro interno |
| 2 | `wait` com caso `cancelled`; `run` respondido com não |
| 3 | `sql` sem `duckdb` no PATH |
| 4 | caso não existe, ou o status não permite (ex.: `run` em caso já terminado sem `--again`, `cancel` fora de `pending`); `wait` sem id sem caso `running` |
| 5 | `run` sem TTY em stdin ou stdout |
| 64 | uso errado (ex.: `add` sem `--why`) |
| 124 | `wait` estourou o `--timeout`; `run` com passo que estourou o timeout por passo |
| 128+n | `run` interrompido pelo sinal n (SIGINT → 130, SIGTERM → 143): caso fica `cancelled` |

O `run` sai com o exit do último passo que rodou.

## Estado

`$GAR_HOME`, padrão `~/.local/state/gar/`:

- `runs/<id>.json` — um arquivo por caso, fonte da verdade, gravado de forma atômica (temp + rename)
  a cada passo, então o `wait` vê o progresso.
- `counter` — último ID, protegido por `flock`. IDs começam em 1001.
- `lock` — serializa mudanças de status entre processos (`run` × `cancel`).

Status do caso: `pending → running → done | failed | cancelled` (ou `pending → cancelled`).
Status do passo: `pending`, `running`, `done`, `failed`, `cancelled`, `skipped`.
`output` guarda a saída combinada do passo sem ANSI, com `\r\n` → `\n`, só os últimos 64 KiB
(`output_truncated: true` quando cortou).

## Build

```sh
CGO_ENABLED=0 go build -o gar .
go vet ./... && go test ./...
```
