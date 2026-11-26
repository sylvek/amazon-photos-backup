# amazon-photos-backup

Sauvegarde en continu, sur votre disque, l'intégralité de votre bibliothèque
Amazon Photos (photos et vidéos, originaux en pleine résolution).

Vous vous authentifiez **une seule fois** en fournissant les cookies de votre
navigateur. Le service tourne ensuite en tâche de fond dans Docker, conserve
la session sur disque, et reprend tout seul après un redémarrage.

> **À savoir.** Amazon Photos n'a pas d'API publique. Ce programme utilise
> l'API interne du site web, non documentée : elle peut changer sans préavis
> et casser l'outil. Amazon peut aussi révoquer une session à tout moment
> (changement de mot de passe, contrôle de sécurité). Dans ce cas le service
> vous prévient et il faut refaire l'étape « connexion » : aucun outil ne
> peut garantir une session éternelle.

## Démarrage

Prérequis : Docker avec le plugin Compose.

### 1. Préparer le dossier

```bash
mkdir -p data
```

```bash
printf 'PUID=%s\nPGID=%s\n' "$(id -u)" "$(id -g)" > .env
```

Le conteneur tourne avec votre utilisateur : les photos vous appartiennent
sur l'hôte. Créez bien `data` vous-même, sinon Docker le crée en `root` et le
service ne pourra pas y écrire.

### 2. Copier les cookies depuis le navigateur

1. Ouvrez une **fenêtre de navigation privée** et connectez-vous sur
   <https://www.amazon.fr/photos> en cochant « Rester connecté ».
2. Ouvrez les outils de développement (F12), onglet **Réseau**, puis rechargez
   la page.
3. Cliquez sur une requête vers `www.amazon.fr` (par exemple une dont l'URL
   contient `drive/v1`), puis dans **En-têtes de la requête** copiez la valeur
   complète de l'en-tête `Cookie`.
4. Collez-la dans un fichier `cookies.txt`.
5. Fermez la fenêtre privée **sans vous déconnecter** : se déconnecter
   invaliderait la session que vous venez de copier.

`document.cookie` dans la console ne suffit pas : le cookie d'authentification
(`at-acbfr`) est `HttpOnly` et n'y apparaît pas.

### 3. Construire l'image et se connecter

```bash
docker compose build
```

```bash
docker compose run --rm -T amazon-photos-backup login < cookies.txt
```

```bash
rm cookies.txt
```

La commande `login` vérifie les cookies auprès d'Amazon avant de les
enregistrer. S'ils sont refusés, rien n'est écrit.

### 4. Lancer le service

```bash
docker compose up -d
```

La première sauvegarde démarre aussitôt. Les suivantes ont lieu toutes les
6 heures. Grâce à `restart: unless-stopped`, le service redémarre avec la
machine, sans nouvelle connexion.

## Suivi

```bash
docker compose logs -f
```

```bash
docker compose exec amazon-photos-backup amazon-photos-backup status
```

`docker ps` affiche aussi l'état de santé du conteneur. Il passe à
`unhealthy` quand la session doit être renouvelée, ou quand aucune sauvegarde
n'a réussi pendant trois intervalles.

| État            | Signification                                                        |
|-----------------|----------------------------------------------------------------------|
| `syncing`       | sauvegarde en cours (la première peut durer longtemps)               |
| `ok`            | tout est sauvegardé                                                  |
| `degraded`      | sauvegarde terminée, mais certains éléments ont échoué (voir `status`) |
| `error`         | la dernière sauvegarde n'a pas pu aboutir ; nouvel essai automatique |
| `auth_required` | Amazon refuse la session : il faut refaire la connexion              |

## Quand la session expire

Le service journalise l'erreur, passe en `auth_required`, envoie une
notification si `NOTIFY_URL` est défini, et attend. Refaites les étapes 2 et 3
(`cookies.txt` puis `login`) : le service en cours d'exécution détecte les
nouveaux cookies en moins de 30 secondes et reprend là où il s'était arrêté.
Inutile de le redémarrer.

## Configuration

Variables d'environnement, à régler dans `docker-compose.yml` :

| Variable               | Défaut         | Rôle                                                            |
|------------------------|----------------|-----------------------------------------------------------------|
| `SYNC_INTERVAL`        | `6h`           | pause entre deux sauvegardes (minimum `5m`)                     |
| `DOWNLOAD_CONCURRENCY` | `4`            | téléchargements simultanés (1 à 16)                             |
| `NOTIFY_URL`           | —              | webhook d'alerte : `POST` en texte brut, compatible [ntfy](https://ntfy.sh) |
| `AMAZON_TLD`           | selon cookies  | site Amazon : `fr`, `de`, `co.uk`, `com`…                       |
| `USER_AGENT`           | Chrome/Linux   | User-Agent présenté à Amazon (idéalement celui de votre navigateur) |
| `TZ`                   | `Europe/Paris` | fuseau horaire des journaux                                     |
| `LOG_LEVEL`            | `info`         | `debug`, `info`, `warn`, `error`                                |

## Ce que fait le service

- **Rangement** : `data/photos/AAAA/MM/<nom d'origine>`, d'après la date de
  prise de vue (à défaut, la date d'envoi ; sinon `unknown/`). La date de
  modification du fichier est celle de la prise de vue.
- **Incrémental** : un fichier déjà sauvegardé n'est jamais retéléchargé.
- **Vérifié** : chaque téléchargement est contrôlé (taille et MD5 annoncés par
  Amazon) avant de recevoir son nom définitif. Un téléchargement interrompu ne
  laisse pas de fichier incomplet sous un nom de photo (après une coupure de
  courant, il peut rester un fichier caché `.dl-…`, sans danger, à supprimer).
  Si Amazon sert deux fois de suite les mêmes octets alors qu'ils ne
  correspondent pas au MD5 annoncé, le fichier est conservé et signalé
  (`mismatched`) plutôt que jeté.
- **Prudent en cas de panne** : disque plein ou volume en lecture seule
  arrêtent la sauvegarde avec une erreur, au lieu de compter chaque fichier
  comme « échoué ». Si 20 téléchargements échouent d'affilée, la passe
  s'arrête et l'état passe à `error`.
- **Jamais destructeur** : rien n'est supprimé ni écrasé en local. Une photo
  supprimée sur Amazon reste dans la sauvegarde. Si deux photos portent le
  même nom, ou si une photo est modifiée sur Amazon, la nouvelle est rangée à
  côté avec un suffixe (`IMG_0001_AbCd1234.jpg`).
- **Réparable** : si un fichier sauvegardé est supprimé localement, il est
  retéléchargé. Si l'index (`data/state/index.json`) est perdu, les fichiers
  présents sont reconnus par leur MD5 au lieu d'être retéléchargés.

## Sécurité

- `data/state/cookies.json` donne accès à votre compte Amazon. Il est créé en
  mode `0600` ; ne le partagez pas et ne le versionnez pas.
- Les cookies ne sont envoyés qu'à `amazon.<tld>` et ses sous-domaines, jamais
  aux serveurs de contenu vers lesquels Amazon redirige les téléchargements.
- Le conteneur tourne sans privilèges, en lecture seule hors de `/data`.

## Limites connues

- L'API de recherche d'Amazon ne renvoie pas plus de 9999 éléments par
  requête. Les grandes bibliothèques sont donc parcourues par année, puis par
  mois, puis par jour. Plus de 9999 éléments sur une même journée ne peuvent
  pas tous être listés : le service le signale (`unreachable`) au lieu de
  l'ignorer.
- Seule votre propre bibliothèque est sauvegardée (pas l'espace famille des
  autres membres).
- Le programme n'a pu être testé que contre un faux serveur Amazon inclus dans
  les tests ; voyez « Développement ».

## Développement

Go 1.24, bibliothèque standard uniquement.

```bash
go test ./...
```

Pour essayer le service sans compte Amazon, un faux serveur est fourni :

```bash
go run ./internal/amazon/amazontest/fakeserver
```

Il affiche une valeur pour `AMAZON_BASE_URL` et des cookies qu'il accepte.
