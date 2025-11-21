import * as path from 'path';
import { promisify } from 'util';
import { readFile } from 'fs';
import { Readable } from 'stream';
import { ConfigService } from '@nestjs/config';
import { ConfigType } from '../../config';
import { pathResolve } from '../../core/utils/files';
import {
  Inject,
  Injectable,
  LoggerService,
  StreamableFile,
} from '@nestjs/common';
import { FileAdapterService } from './file.adapter.service';
import { FileInfoInputData } from './inputs';
import { FileInfoOutputData, isFileInfoOutputWithErrorData } from './outputs';
import {
  FileInfoException,
  FileReadException,
  LocalStorageReadFailedException,
} from './exceptions';
import { DocumentData } from './types';
import { WINSTON_MODULE_NEST_PROVIDER } from 'nest-winston';

const readFileAsync = promisify(readFile);

@Injectable()
export class FileService {
  private readonly storagePath: string;

  constructor(
    @Inject(WINSTON_MODULE_NEST_PROVIDER)
    private readonly logger: LoggerService,
    private readonly adapter: FileAdapterService,
    private readonly configService: ConfigService<ConfigType, true>,
  ) {
    const { storage_path } = this.configService.get(
      'settings.application.storage',
      { infer: true },
    );
    this.storagePath = pathResolve(storage_path);
    this.logger.verbose?.(`Serving files from ${this.storagePath}`);
  }

  public isConnected(): Promise<boolean> {
    return this.adapter.isConnected();
  }

  public async fileInfo(
    docId: string,
    auth: {
      cookie?: string;
      authorization?: string;
      guestName?: string
    },
  ): Promise<FileInfoOutputData> {
    const { cookie, authorization, guestName } = auth;

    return this.adapter.fileInfo(
      new FileInfoInputData(docId, { cookie, authorization, guestName }),
    );
  }

  /**
   *
   * @param docId
   * @param auth
   * @throws FileInfoException
   * @throws FileReadException
   * @throws LocalStorageReadFailedException
   */
  public async readDocument(
    docId: string,
    auth: {
      cookie?: string;
      authorization?: string;
      guestName?: string
    },
  ): Promise<DocumentData | never> {
    const { data } = await this.fileInfo(docId, auth);

    if (isFileInfoOutputWithErrorData(data)) {
      throw new FileInfoException(
        'Error while reading file info',
        data.errorCode,
        {
          originalMessage: data.error,
        },
      );
    }

    const fileStream = await this.readFileToStream(data.fileName);

    return {
      file: fileStream,
      mimeType: data.mimeType,
    };
  }

  /**
   *
   * @param fileName
   * @throws FileReadException
   * @throws LocalStorageReadFailedException
   */
  public readFileToStream(fileName: string): Promise<StreamableFile> {
    if (!fileName) {
      throw new FileReadException('File name not provided');
    }

    const filePath = this.getFilePath(fileName);
    return localStorageFileToStream(filePath);
  }

  private getFilePath(fileName: string): string {
    return path.join(this.storagePath, fileName);
  }
}

/**
 * Converts a file on the local storage to a {@link StreamableFile}
 * @param filePath
 * @throws LocalStorageReadFailedException
 */
const localStorageFileToStream = async (
  filePath: string,
): Promise<StreamableFile | never> => {
  try {
    const contents = await readFileAsync(filePath);
    const readable = Readable.from(contents);
    return new StreamableFile(readable);
  } catch (e: any) {
    throw new LocalStorageReadFailedException('Unable to read file', {
      message: e?.message,
      originalError: e,
      filePath,
    });
  }
};
