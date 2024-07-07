import { WINSTON_MODULE_NEST_PROVIDER } from 'nest-winston';
import { firstValueFrom, map, timeInterval, timeout } from 'rxjs';
import { Inject, Injectable, LoggerService } from '@nestjs/common';
import { ConfigService } from '@nestjs/config';
import {
  ClientProxyFactory,
  RmqOptions,
  Transport,
} from '@nestjs/microservices';
import { ConfigType } from 'src/config';
import { FileMessagePatternEnum } from './file.message.pattern.enum';
import { FileInfoInputData } from './inputs';
import { FileInfoOutputData, HealthCheckOutputData } from './outputs';
import { ClientRMQ } from '@nestjs/microservices/client/client-rmq';
import { RMQConnectionError } from './types';

@Injectable()
export class FileAdapterService {
  private readonly client: ClientRMQ | undefined;
  private readonly timeoutMs: number;

  constructor(
    @Inject(WINSTON_MODULE_NEST_PROVIDER) private logger: LoggerService,
    private readonly configService: ConfigService<ConfigType, true>,
  ) {
    const rabbitMqOptions = this.configService.get('rabbitmq.connection', {
      infer: true,
    });
    const queue = this.configService.get('settings.application.auth_queue', {
      infer: true,
    });

    this.client = authQueueClientProxyFactory(
      {
        ...rabbitMqOptions,
        queue,
      },
      this.logger,
    );
    this.timeoutMs = this.configService.get(
      'settings.application.response_timeout',
      { infer: true },
    );
  }

  public async startConnection(): Promise<void> {
    if (!this.client) {
      this.logger.error(`${FileAdapterService.name} not initialized`);
      return;
    }
    this.client
      .connect()
      .then(() => {
        this.logger.verbose?.(
          'Client proxy successfully connected to RabbitMQ',
        );
      })
      .catch(({ err }: RMQConnectionError) =>
        this.logger.error(err, err.stack),
      );
  }

  /**
   * Is there a healthy connection to the queue
   */
  public async isConnected(): Promise<boolean> {
    return this.sendWithResponse<HealthCheckOutputData, string>(
      FileMessagePatternEnum.HEALTH_CHECK,
      'healthy?',
      { timeoutMs: 3000 },
    )
      .then((resp) => resp.healthy)
      .catch(() => false);
  }

  /**
   * Give information about a file. Access of the requester to that file and metadata.
   * This method sends a request to the queue and waits for a response.
   *
   * @param {FileInfoInputData} data - The read request data.
   * @returns {Promise<FileInfoOutputData | never>} - Returns a promise that resolves or throws an error.
   * @throws Error
   */
  public fileInfo(
    data: FileInfoInputData,
  ): Promise<FileInfoOutputData | never> {
    return this.sendWithResponse<FileInfoOutputData, FileInfoInputData>(
      FileMessagePatternEnum.FILE_INFO,
      data,
    );
  }

  /**
   * Sends a message to the queue and waits for a response.
   * Each consumer needs to manually handle failures, returning the proper type.
   * @param pattern
   * @param data
   * @param options
   * @throws Error
   */
  private sendWithResponse = async <TResult, TInput>(
    pattern: FileMessagePatternEnum,
    data: TInput,
    options?: { timeoutMs?: number },
  ): Promise<TResult | never> => {
    if (!this.client) {
      throw new Error(`Connection was not established. Send failed.`);
    }

    const timeoutMs = options?.timeoutMs ?? this.timeoutMs;

    const result$ = this.client.send<TResult, TInput>(pattern, data).pipe(
      timeInterval(),
      map((x) => {
        this.logger.debug?.({
          method: `sendWithResponse took ${x.interval}ms`,
          pattern,
          data,
          value: x.value,
        });
        return x.value;
      }),
      timeout({ each: timeoutMs }),
    );

    return firstValueFrom(result$).catch(({ err }: RMQConnectionError) => {
      this.logger.error(
        err.message,
        err.stack,
        JSON.stringify({
          pattern,
          data,
          timeout: timeoutMs,
        }),
      );

      throw new Error('Error while processing request.');
    });
  };
}

const authQueueClientProxyFactory = (
  config: {
    user: string;
    password: string;
    host: string;
    port: number;
    heartbeat: number;
    queue: string;
  },
  logger: LoggerService,
): ClientRMQ | undefined => {
  const { host, port, user, password, heartbeat: _heartbeat, queue } = config;
  const heartbeat =
    process.env.NODE_ENV === 'production' ? _heartbeat : _heartbeat * 3;
  logger.verbose?.({ ...config, heartbeat, password: undefined });
  try {
    const options: RmqOptions = {
      transport: Transport.RMQ,
      options: {
        urls: [
          {
            protocol: 'amqp',
            hostname: host,
            username: user,
            password,
            port,
            heartbeat,
          },
        ],
        queue,
        queueOptions: { durable: true },
        noAck: true,
      },
    };
    return ClientProxyFactory.create(options) as ClientRMQ;
  } catch (err) {
    logger.error(`Could not connect to RabbitMQ: ${err}`);
    return undefined;
  }
};
