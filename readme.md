# w3se

w3se es un bridge de conexiones persistentes que permite a un backend HTTP síncrono comunicarse con clientes que requieren conexiones SSE (Server-Sent Events) o WebSocket.

Funciona como una capa de infraestructura entre dos mundos: los clientes que necesitan mantener una conexión abierta para recibir datos en tiempo real, y los backends que operan con el modelo tradicional de request-response.

w3se no procesa ni transforma los mensajes que retransmite. Es un componente de transporte genérico que puede usarse para notificaciones en tiempo real, dashboards con datos en vivo, chat, monitoreo, streaming de resultados de procesamiento largo, o cualquier escenario que requiera push de datos desde el servidor hacia clientes conectados.

## Manual del usuario

El manual de usuario está disponible aquí: <https://docs.induxsoft.net/es/productos/tools/w3se/>

## Código fuente

Todo el código está escrito en Go, disponible en la carpeta [src]() 