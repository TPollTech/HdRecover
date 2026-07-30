# Clonagem e migração

## Estado

As funções de clonagem continuam **experimentais** até a conclusão da matriz de testes físicos. Os testes automatizados reduzem riscos de regressão, mas não substituem validação com controladores, firmwares e discos reais.

## Modos

### Clone setor a setor offline

Usado para disco secundário. Todos os volumes montados da origem e do destino precisam ser bloqueados e desmontados. Se qualquer bloqueio falhar, nenhuma gravação começa.

### Migração do Windows em uso com VSS

Usada quando a origem contém o Windows ativo. Todos os volumes montados da origem precisam ser mapeados para o mesmo disco e cobertos por snapshots VSS. O modo é bloqueado se o BitLocker não estiver totalmente descriptografado ou não puder ser consultado.

### Migração para SSD menor

O programa analisa GPT/MBR e reduz temporariamente apenas a última partição NTFS redimensionável que ultrapassa o limite. Somente partições explicitamente identificadas como recuperação Windows/OEM podem ser omitidas. O plano é salvo e revalidado antes da redução.

## Barreiras de segurança

1. Atualize a lista de discos imediatamente antes da operação.
2. Confirme modelo, tamanho, serial e `UniqueId`.
3. Salve relatórios fora do destino; no modo offline, também fora da origem.
4. Digite `CLONAR` ou `MIGRAR` conforme solicitado.
5. Revise a confirmação final que identifica qual disco será apagado.
6. Não reconecte, troque adaptadores nem altere partições após confirmar.
7. Após migrar o Windows, desligue o computador e faça o primeiro boot somente com o novo SSD.
8. Não reutilize o disco antigo antes de validar boot, arquivos e relatórios.

## Retomada

A retomada offline só é habilitada quando origem e destino possuem serial ou `UniqueId`. O estado aponta apenas para blocos cujo conteúdo, manifesto e dados de destino já foram sincronizados. O último bloco confirmado é verificado por SHA-256 antes de continuar.

Migrações VSS e para SSD menor não são retomadas: após interrupção, devem começar do zero.

## GPT e MBR

- GPT: cabeçalho primário e tabela de entradas precisam ter CRC válido antes do ajuste.
- A GPT secundária é recriada no fim físico do destino.
- O MBR protetor é atualizado ou recriado somente quando há entrada disponível.
- MBR: partições essenciais que ultrapassam o destino bloqueiam a migração.
- Apenas tipos explícitos de recuperação podem ser omitidos.

## Matriz mínima para liberar uma versão estável

| Cenário | Critério |
|---|---|
| GPT/UEFI, destino maior | Boot, partições e verificação aprovados |
| GPT/UEFI, SSD menor | Redução, restauração da origem e boot aprovados |
| MBR/BIOS | Boot e geometria aprovados |
| Setores 512 e 4K | Tamanho lógico igual e cópia verificada |
| Setores defeituosos simulados | Zero-fill e mapa de erros corretos |
| Interrupção durante clone offline | Retomada no checkpoint confirmado |
| Troca de destino após confirmação | Operação bloqueada |
| BitLocker ativo ou indeterminado | Migração VSS bloqueada |
| Falha parcial de VSS | Operação bloqueada |

Cada teste físico deve usar hardware descartável, imagens sintéticas e hashes conhecidos. Não use dados únicos nem discos de clientes.
