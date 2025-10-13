# Быстрый старт - Windows Parental Control Bot

## 1. Подготовка

1. **Скопируйте файлы на Windows машину:**
   - `parental-control-bot.exe`
   - `config.json.example`

2. **Настройте конфигурацию:**
   ```cmd
   copy config.json.example config.json
   notepad config.json
   ```

3. **Отредактируйте `config.json`:**
   - Замените `YOUR_BOT_TOKEN_HERE` на токен вашего бота
   - Замените `123456789` на ваш Telegram ID
   - Настройте детские аккаунты

## 2. Тестирование

**Проверьте конфигурацию:**
```cmd
parental-control-bot.exe -test
```

**Запустите в отладочном режиме:**
```cmd
parental-control-bot.exe -debug
```

Если все работает - можете тестировать бота в Telegram!

## 3. Установка как сервис

**Установите сервис (от имени администратора):**
```cmd
parental-control-bot.exe -install
```

**Проверьте статус:**
```cmd
sc query "Parental Control Bot Service"
```

## 4. Использование

1. Откройте Telegram
2. Найдите вашего бота
3. Отправьте `/start`
4. Используйте кнопки для управления

## Команды для отладки

```cmd
# Тест конфигурации
parental-control-bot.exe -test

# Отладочный режим
parental-control-bot.exe -debug

# Установка сервиса
parental-control-bot.exe -install

# Удаление сервиса
parental-control-bot.exe -uninstall

# Справка
parental-control-bot.exe
```

## Если что-то не работает

1. **Сначала:** `parental-control-bot.exe -test`
2. **Потом:** `parental-control-bot.exe -debug`
3. **Проверьте:** Windows Event Log
4. **Убедитесь:** Запуск от имени администратора

## Получение Telegram Bot Token

1. Найдите [@BotFather](https://t.me/BotFather) в Telegram
2. Отправьте `/newbot`
3. Следуйте инструкциям
4. Скопируйте полученный токен в `config.json`

## Получение вашего Telegram ID

1. Найдите [@userinfobot](https://t.me/userinfobot) в Telegram
2. Отправьте любое сообщение
3. Скопируйте ваш ID в `config.json`
