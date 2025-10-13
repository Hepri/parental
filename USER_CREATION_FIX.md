# Исправление ошибки 2221 при создании пользователей

## Проблема

Ошибка `NewUserWithInfo failed with code 2221` возникает при попытке создать пользовательские аккаунты через Windows API. Код ошибки 2221 означает "Invalid computer name or insufficient privileges".

## Причины

1. **Недостаточные права доступа** - приложение должно запускаться от имени администратора
2. **Проблемы с Windows API** - NetUserAdd может работать некорректно в некоторых версиях Windows
3. **Неправильные параметры** - некорректная структура данных для NetUserAdd

## Решение

### 1. Улучшенная обработка ошибок

Добавлена функция `getNetApiErrorMessage()` для детального описания ошибок:

```go
func getNetApiErrorMessage(errorCode uintptr) string {
    switch errorCode {
    case 2221:
        return "Invalid computer name or insufficient privileges"
    case 2224:
        return "User already exists"
    case 2225:
        return "User does not exist"
    case 2226:
        return "Password too short or does not meet complexity requirements"
    case 5:
        return "Access denied - run as administrator"
    case 87:
        return "Invalid parameter"
    case 1314:
        return "A required privilege is not held by the client"
    default:
        return fmt.Sprintf("Unknown error code: %d", errorCode)
    }
}
```

### 2. Альтернативный метод создания пользователей

Если NetUserAdd не работает, используется команда `net.exe`:

```go
func createUserAccountAlternative(account ChildAccount) error {
    // Создание пользователя через net.exe
    cmd := exec.Command("net", "user", account.Username, account.Password, 
        "/add", "/fullname:"+account.FullName, 
        "/passwordchg:no", "/expires:never")
    output, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("net user command failed: %v, output: %s", err, string(output))
    }
    
    // Добавление в группу Users
    cmd = exec.Command("net", "localgroup", "Users", account.Username, "/add")
    output, err = cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("net localgroup command failed: %v, output: %s", err, string(output))
    }
    
    return nil
}
```

### 3. Улучшенная проверка существования пользователей

Добавлен fallback метод через `net user`:

```go
func userExists(username string) (bool, error) {
    // Сначала пробуем NetUserGetInfo
    // Если не работает, используем net user
    cmd := exec.Command("net", "user", username)
    output, err := cmd.CombinedOutput()
    // ... проверка вывода
}
```

### 4. Автоматический fallback

Теперь при создании пользователей:

1. Сначала пробуем NetUserAdd API
2. Если не работает (ошибка 2221), автоматически переключаемся на `net.exe`
3. Выводим понятные сообщения о том, какой метод использовался

## Как использовать

### Для диагностики:

```cmd
# Проверка конфигурации
parental-control-bot.exe -test

# Отладочный режим с подробными логами
parental-control-bot.exe -debug
```

### Требования:

1. **Запуск от имени администратора** - обязательно для создания пользователей
2. **Права на создание пользователей** - учетная запись должна быть в группе Administrators
3. **Политики безопасности** - убедитесь, что политики не блокируют создание пользователей

## Примеры ошибок и решений

### Ошибка 2221:
```
NetUserAdd failed with code 2221: Invalid computer name or insufficient privileges
✓ Created user account using alternative method: child1
```

### Ошибка 5 (Access Denied):
```
NetUserAdd failed with code 5: Access denied - run as administrator
```

**Решение:** Запустите приложение от имени администратора

### Ошибка 2226 (Password complexity):
```
NetUserAdd failed with code 2226: Password too short or does not meet complexity requirements
```

**Решение:** Пароли генерируются автоматически с соблюдением требований сложности

## Логирование

Теперь приложение выводит подробную информацию:

```
✓ User account already exists: child1
✓ Created user account: child2
✓ Created user account using alternative method: child3
```

Это помогает понять, какой метод создания пользователей работает в вашей системе.
