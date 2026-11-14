# gar — como o agente usa

Quando você precisa que o humano rode um comando (ssh bloqueado pelo guard, senha, sudo):

1. Registre: `gar add --why "<por quê>" [--expect "<o que deve aparecer>"] -- '<cmd1>' '<cmd2>'`.
   Cada argumento depois de `--` é um passo; eles rodam em ordem e param no primeiro que falhar.
2. Diga ao humano a linha que o `add` imprimiu. Dentro do bora ou do Herdr app ela é só `! gar run`:
   o humano escolhe o caso com setas e Enter, sem copiar id. Fora deles vem `! gar run <id>`.
3. Rode `gar wait <id>` em background (`--timeout` se quiser limite). Ele sai com o JSON do caso:
   exit 0 `done`, 1 `failed`, 2 `cancelled`, 124 timeout. Leia `steps[].exit_code` e `steps[].output`.

Nunca rode `gar run`: ele recusa sem TTY (exit 5) de propósito, e quem roda é o humano.
Desistiu do caso antes de o humano rodar? `gar cancel <id>`.
Consultar: `gar show <id>`, `gar ls --status pending`.

## Desenvolvimento

Go puro (`CGO_ENABLED=0`), deps só `creack/pty` e `golang.org/x/term`, `flag` da stdlib.
`go vet ./... && go test ./...` antes de commitar. O teste de integração sobe `gar run` num PTY real.
`GAR_HERDR_DIALOG=1` (só o mod `gar-run` do Herdr desktop põe): no seletor, `Esc` roda `herdr-app do overlay.close` e segue esperando, e o caso escolhido ganha `hc label $HC_TERMINAL_ID gar.case=<id>`.
