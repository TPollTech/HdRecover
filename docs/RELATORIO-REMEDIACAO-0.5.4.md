# Relatório de remediação — 0.5.4-rc.1

Data: 30/07/2026

## Resultado

| Achado da auditoria | Tratamento |
|---|---|
| Pacote de fonte corrompido | Removido; fonte publicada como arquivos normais |
| Versão anterior removida | Preservada em `preservation/hdrecover-0.5.3-bin`, sem aprovação |
| Motor não validável | Fonte e testes restaurados; proteções críticas reforçadas |
| Workflows com escrita | Removidos |
| PR #1 não entrega o descrito | Deve ser fechado em favor do PR de remediação |
| Ausência de CI | CI somente leitura adicionada |
| Ausência de documentação | README, SECURITY, CHANGELOG e guia de clonagem adicionados |

## Controles implementados

- Revalidação de identidade e função de sistema dos discos.
- Bloqueio fail-closed de volumes no clone offline.
- VSS fail-closed e validação de BitLocker.
- Revalidação do plano antes de reduzir a origem.
- Restauração vinculada à identidade física da origem.
- Retomada com checkpoint durável e identidade de hardware.
- Validação de CRC GPT e classificação restrita de partições descartáveis.
- Builds reproduzíveis com hashes, sem binários versionados.

## Limite da remediação

O código e a automação podem ser validados em CI, mas esta remediação não representa aprovação em hardware real. A versão deve permanecer `rc` e experimental até que a matriz em `docs/CLONAGEM.md` seja executada e documentada.
