# HdRecover

Ferramenta de recuperação de arquivos, criação de imagens e clonagem de discos para Windows.

> **Estado atual: 0.5.4-rc.1, experimental.** O código voltou a estar disponível como arquivos normais no Git e possui testes automatizados. Ainda não existe executável aprovado para uso profissional em discos de clientes, pois a validação física controlada permanece pendente.

## Aviso de segurança

A clonagem escreve diretamente no disco de destino e apaga seu conteúdo. Confira modelo, capacidade, serial e `UniqueId` antes de confirmar. Mantenha backup independente e valide primeiro com imagens virtuais ou hardware descartável.

O programa bloqueia a operação quando:

- origem e destino não podem ser reidentificados imediatamente antes da gravação;
- o destino possui função de boot ou sistema do Windows em uso;
- origem e destino são o mesmo disco;
- o destino está marcado como somente leitura;
- o modo offline não consegue bloquear e desmontar todos os volumes;
- o VSS não consegue proteger todos os volumes montados da origem;
- BitLocker está ativo, em transição ou não pode ser consultado no modo de migração;
- o mapa de partições muda depois da confirmação;
- o relatório local não pode ser associado com segurança a um disco físico.

## Plataformas

- Interface e acesso a discos físicos: Windows 10 e Windows 11, executados como administrador.
- Motor de recuperação e testes unitários: Go 1.23 ou posterior.
- Builds oficiais de CI: Windows `amd64` e `386`.

## Compilar

Instale Go e execute:

```powershell
.\build.ps1
```

Os executáveis e seus hashes SHA-256 serão criados em `dist\`. A compilação não torna o aplicativo automaticamente aprovado para uso profissional.

## Validar

```powershell
gofmt -w .
go test ./...
go test -race ./...
go vet ./...
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0
govulncheck ./...
```

Cada pull request executa testes, análise estática, detector de concorrência, verificação de vulnerabilidades e builds Windows. A CI possui apenas permissão de leitura e não cria commits.

## Clonagem e migração

Consulte [docs/CLONAGEM.md](docs/CLONAGEM.md) para modos suportados, restrições e critérios de aprovação.

## Histórico preservado

A distribuição binária anterior foi preservada na branch `preservation/hdrecover-0.5.3-bin` somente para rastreabilidade. Ela não deve ser tratada como versão auditada ou recomendada.

## Segurança

Para reportar uma vulnerabilidade, siga [SECURITY.md](SECURITY.md). Mudanças relevantes estão em [CHANGELOG.md](CHANGELOG.md).
